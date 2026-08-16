package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blairham/gish/internal/history"
)

// jumpEnv installs a jumpManager over temp state with two known dirs.
func jumpEnv(t *testing.T) (work, api string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	work = filepath.Join(base, "projects", "gish")
	api = filepath.Join(base, "services", "api")
	for _, d := range []string{work, api} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mgr := newJumpManager(nil)
	if mgr == nil {
		t.Fatal("jump manager unavailable")
	}
	mgr.store.Visit(work, time.Now())
	mgr.store.Visit(api, time.Now())
	jumpMgr = mgr
	t.Cleanup(func() { jumpMgr = nil })
	return work, api
}

func TestZJumpsToBestMatch(t *testing.T) {
	_, api := jumpEnv(t)
	rc := filepath.Join(t.TempDir(), "gishrc")
	out, _, err := runConfigScript(t, rc, "z api\npwd\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, api) {
		t.Errorf("z did not move the session: %q", out)
	}
}

func TestZListAndNoMatch(t *testing.T) {
	work, api := jumpEnv(t)
	rc := filepath.Join(t.TempDir(), "gishrc")
	out, _, err := runConfigScript(t, rc, "z -l\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, work) || !strings.Contains(out, api) {
		t.Errorf("z -l missing dirs: %q", out)
	}

	_, errOut, _ := runConfigScript(t, rc, "z nonexistent-xyz\n")
	if !strings.Contains(errOut, "no match") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestZBarePickerDegradesToList(t *testing.T) {
	work, _ := jumpEnv(t)
	rc := filepath.Join(t.TempDir(), "gishrc")
	// Headless: the picker degrades to the ranked list, nothing moves.
	out, _, err := runConfigScript(t, rc, "z\npwd\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, work) {
		t.Errorf("bare z should list top dirs: %q", out)
	}
}

func TestJumpBootstrapFromHistory(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	worked := filepath.Join(base, "worked-here")
	if err := os.MkdirAll(worked, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(base, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck // test cleanup
	for range 3 {
		if _, err := store.Append(history.Entry{Command: "make test", Cwd: worked}); err != nil {
			t.Fatal(err)
		}
	}

	mgr := newJumpManager(store)
	if mgr == nil {
		t.Fatal("jump manager unavailable")
	}
	got := mgr.store.Query([]string{"worked"}, time.Now())
	if len(got) != 1 || got[0].Dir != worked {
		t.Errorf("history bootstrap missing: %+v", got)
	}
}

func TestJumpNoteRespectsOptOut(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	mgr := newJumpManager(nil)
	runner := newTestRunner(t)
	if err := runEnvScript(t.Context(), runner, "GISH_JUMP=off\n"); err != nil {
		t.Fatal(err)
	}
	mgr.note(runner)
	if !mgr.store.Empty() {
		t.Error("GISH_JUMP=off still recorded a visit")
	}
	if err := runEnvScript(t.Context(), runner, "GISH_JUMP=on\n"); err != nil {
		t.Fatal(err)
	}
	mgr.note(runner)
	if mgr.store.Empty() {
		t.Error("visit not recorded")
	}
}
