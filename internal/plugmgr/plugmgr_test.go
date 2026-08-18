package plugmgr

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/blairham/koi-shell/internal/plugmgr/ice"
	"github.com/blairham/koi-shell/internal/plugmgr/spec"
	"github.com/blairham/koi-shell/internal/plugmgr/state"
)

// fakeHome builds a ZI_GO_HOME with n manifest-only plugin objects:
// not git repos, not releases, not snippets, so Update takes the
// no-network "skipped" path — which is exactly what a hermetic
// concurrency test wants.
func fakeHome(t *testing.T, n int) *Zi {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ZI_GO_HOME", home)
	z, err := NewZi(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		s, err := spec.ParsePlugin(fmt.Sprintf("user/plug%02d", i), "", "")
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(z.cfg.PluginsDir(), s.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := state.SaveObject(dir, s, ice.New()); err != nil {
			t.Fatal(err)
		}
	}
	return z
}

func TestUpdateAllPrintsInListingOrder(t *testing.T) {
	z := fakeHome(t, 20) // more objects than workers: real queueing

	var out strings.Builder
	if err := z.Update("", &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 20 {
		t.Fatalf("got %d lines:\n%s", len(lines), out.String())
	}
	for i, line := range lines {
		wantID := fmt.Sprintf("user---plug%02d", i)
		if !strings.HasPrefix(line, wantID) {
			t.Errorf("line %d = %q, want prefix %q (listing order lost)", i, line, wantID)
		}
		if !strings.Contains(line, "skipped") {
			t.Errorf("line %d = %q, want the no-network skip outcome", i, line)
		}
	}
}

func TestUpdateSingleTarget(t *testing.T) {
	z := fakeHome(t, 3)

	var out strings.Builder
	if err := z.Update("user/plug01", &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "user---plug01") {
		t.Errorf("out = %q, want exactly the one target", out.String())
	}
}

func TestUpdateReportsPerObjectErrors(t *testing.T) {
	z := fakeHome(t, 2)
	// Corrupt one manifest: its line must carry the error while the
	// other object still updates.
	bad := filepath.Join(z.cfg.PluginsDir(), "user---plug00", ".zi-go.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := z.Update("", &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "reinstall with delete + load") {
		t.Errorf("broken object's line = %q, want the manifest error", lines[0])
	}
	if !strings.Contains(lines[1], "skipped") {
		t.Errorf("healthy object's line = %q", lines[1])
	}
}

func TestUpdateUnknownTarget(t *testing.T) {
	z := fakeHome(t, 1)
	if err := z.Update("no-such-thing", io.Discard); err == nil {
		t.Error("want an error for an unknown target")
	}
}

func TestUpdateWithProgressEvents(t *testing.T) {
	z := fakeHome(t, 5)

	var mu sync.Mutex
	var events []UpdateEvent
	var out strings.Builder
	err := z.UpdateWithProgress("", &out, func(ev UpdateEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	// The observer is the display: no line output.
	if out.Len() != 0 {
		t.Errorf("line output despite observer: %q", out.String())
	}

	// Queued events arrive first, one per object, in listing order.
	for i := range 5 {
		if events[i].State != UpdateQueued || events[i].Index != i {
			t.Fatalf("event %d = %+v, want queued index %d", i, events[i], i)
		}
		if events[i].Name == "" {
			t.Errorf("queued event %d has no name", i)
		}
	}
	// Every object starts and finishes exactly once, with an outcome.
	started, done := map[int]int{}, map[int]int{}
	for _, ev := range events[5:] {
		switch ev.State {
		case UpdateStarted:
			started[ev.Index]++
		case UpdateDone:
			done[ev.Index]++
			if ev.Outcome == "" {
				t.Errorf("done event %+v has no outcome", ev)
			}
		case UpdateQueued:
			t.Errorf("queued event after work began: %+v", ev)
		}
	}
	for i := range 5 {
		if started[i] != 1 || done[i] != 1 {
			t.Errorf("object %d: started %d, done %d", i, started[i], done[i])
		}
	}
}
