//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// KOI_TRACE_JSON (#474): a script session appends one JSON object per
// executed simple command to the named file, and the script's own
// stdout/stderr are byte-identical to an untraced run — tracing that
// leaked into the output would change what it observes.
func TestJSONTraceEndToEnd(t *testing.T) {
	koiBin := buildKoi(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "s.sh")
	src := "x=5\necho out $x\nfalse\nsleep 0.2\necho oops >&2\n"
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(dir, "trace.jsonl")
	env := []string{
		"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "HOME=" + t.TempDir(),
		"KOI_TRACE_JSON=" + tracePath,
	}
	out, code := runArgvEnv(t, koiBin, []string{script}, env)
	if out != "out 5\n" || code != 0 {
		t.Fatalf("script output = %q (exit %d), want %q (exit 0)", out, code, "out 5\n")
	}

	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("no trace file written: %v", err)
	}
	var events []interp.TraceEvent
	for line := range strings.Lines(strings.TrimSuffix(string(raw), "\n")) {
		var ev interp.TraceEvent
		if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &ev); err != nil {
			t.Fatalf("trace line is not JSON: %q: %v", line, err)
		}
		events = append(events, ev)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	echo := events[0]
	if got := strings.Join(echo.Expanded, " "); got != "echo out 5" {
		t.Errorf("first event argv = %q, want the expanded echo", got)
	}
	if echo.Cmd != "echo out $x" || echo.Line != 2 || echo.Src != script {
		t.Errorf("first event = %+v, want unexpanded text at %s:2", echo, script)
	}
	if events[1].Exit != 1 {
		t.Errorf("false traced exit %d, want 1", events[1].Exit)
	}
	if sleep := events[2]; sleep.DurationMs < 100 {
		t.Errorf("sleep 0.2 traced %dms, want >=100 — duration is not being measured", sleep.DurationMs)
	}

	// Without the variable no file appears: tracing is opt-in.
	otherTrace := filepath.Join(dir, "untraced.jsonl")
	out2, _ := runArgvEnv(t, koiBin, []string{script}, env[:3])
	if out2 != "out 5\n" {
		t.Fatalf("untraced run output = %q", out2)
	}
	if _, err := os.Stat(otherTrace); !os.IsNotExist(err) {
		t.Errorf("trace file exists without KOI_TRACE_JSON set")
	}
}
