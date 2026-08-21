package interp_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// runTraced runs src with a TraceHook installed and returns the events.
// The collector is mutex-guarded because pipeline stages trace
// concurrently — the hook's documented contract.
func runTraced(t *testing.T, src string, files map[string]string) []interp.TraceEvent {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "main.sh")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []interp.TraceEvent
	r, err := interp.New(
		interp.Dir(dir),
		interp.StdIO(nil, io.Discard, io.Discard),
		interp.TraceHook(func(ev interp.TraceEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Run(context.Background(), file)
	return events
}

// argvString joins an event's expanded argv for compact comparison.
func argvString(ev interp.TraceEvent) string { return strings.Join(ev.Expanded, " ") }

func TestTraceHookRecordsExecutedCommands(t *testing.T) {
	t.Parallel()
	events := runTraced(t, `msg=hi
echo one $msg
false
f() { echo in; }
f
(echo sub)
eval 'echo evd'
`, nil)

	// The hook fires when a command *returns*, so a function's body
	// traces before the call that entered it — the order below is the
	// completion order, and the bare assignment on line 1 is absent
	// because only simple commands trace in v1.
	want := []struct {
		argv string
		exit int
		line uint
		fn   string
	}{
		{"echo one hi", 0, 2, ""},
		{"false", 1, 3, ""},
		{"echo in", 0, 4, "f"},
		{"f", 0, 5, ""},
		{"echo sub", 0, 6, ""},
		{"echo evd", 0, 1, ""}, // eval re-parses: line 1 of its own text
		{"eval echo evd", 0, 7, ""},
	}
	if len(events) != len(want) {
		for _, ev := range events {
			t.Logf("event: %+v", ev)
		}
		t.Fatalf("got %d events, want %d", len(events), len(want))
	}
	for i, w := range want {
		ev := events[i]
		if argvString(ev) != w.argv {
			t.Errorf("event %d argv = %q, want %q", i, argvString(ev), w.argv)
		}
		if ev.Exit != w.exit {
			t.Errorf("event %d (%s) exit = %d, want %d", i, w.argv, ev.Exit, w.exit)
		}
		if w.argv != "echo evd" && ev.Line != w.line {
			t.Errorf("event %d (%s) line = %d, want %d", i, w.argv, ev.Line, w.line)
		}
		if ev.Func != w.fn {
			t.Errorf("event %d (%s) func = %q, want %q", i, w.argv, ev.Func, w.fn)
		}
		if ev.StartedUnixMs <= 0 || ev.DurationMs < 0 {
			t.Errorf("event %d (%s) timing = %d/%d, want positive start and non-negative duration",
				i, w.argv, ev.StartedUnixMs, ev.DurationMs)
		}
	}
	// The command text is the source, not the expansion.
	if got := events[0].Cmd; got != "echo one $msg" {
		t.Errorf("event 0 cmd = %q, want the unexpanded source", got)
	}
}

func TestTraceHookFollowsSource(t *testing.T) {
	t.Parallel()
	events := runTraced(t, ". ./lib.sh\n", map[string]string{
		"lib.sh": "echo libbed\n",
	})
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	// The sourced file's command traces with the file it lives in, as
	// written — the BASH_SOURCE rule (#266) — and the `.` line itself
	// traces from the sourcing script.
	if events[0].Src != "./lib.sh" || argvString(events[0]) != "echo libbed" {
		t.Errorf("sourced event = %+v, want echo libbed in ./lib.sh", events[0])
	}
	if events[1].Src != "main.sh" || events[1].Expanded[0] != "." {
		t.Errorf("sourcing event = %+v, want the dot line in main.sh", events[1])
	}
}

func TestTraceHookSeesConcurrentPipelineStages(t *testing.T) {
	t.Parallel()
	events := runTraced(t, "echo p | :\n", nil)
	var got []string
	for _, ev := range events {
		got = append(got, argvString(ev))
	}
	for _, want := range []string{"echo p", ":"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no event for pipeline stage %q; got %v", want, got)
		}
	}
}
