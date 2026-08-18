//go:build unix

package jobs_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/creack/pty"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/jobs"
)

// The capture wiring end to end through the exec path: enable, run a
// line, and the line's output is both on screen and retained.
func runCaptured(t *testing.T, script string, enable bool) (out string, truncated bool) {
	t.Helper()
	table := jobs.NewTable(nil) // no tty: no foreground handoff in a test
	if enable {
		table.EnableCapture(0)
	}
	runner, err := interp.New(
		interp.StdIO(nil, os.Stdout, os.Stdout),
		interp.ExecHandlers(table.ExecMiddleware),
	)
	if err != nil {
		t.Fatal(err)
	}
	table.BeginLine(script)
	file, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background(), file); err != nil {
		t.Fatalf("run: %v", err)
	}
	table.EndLine()
	b, trunc := table.LastCapture()
	return string(b), trunc
}

// Capture is off by default, and off must mean nothing is retained —
// this is an opt-in feature and a shell that quietly recorded output
// would be a surprise.
func TestCaptureOffRetainsNothing(t *testing.T) {
	out, _ := runCaptured(t, "echo hello", false)
	if out != "" {
		t.Errorf("capture was off but retained %q", out)
	}
}

// With no tty the table cannot open a capture pty sized to a real
// terminal, but it still must not break the command.
func TestCaptureWithoutTTYStillRuns(t *testing.T) {
	if out, _ := runCaptured(t, "echo hello", true); out != "" {
		t.Logf("captured %q without a tty", out) // either is acceptable
	}
}

// The seam stages 3 and 4 will consume: with a real terminal and
// capture on, a line's output must actually come back from
// LastCapture. Nothing in the shell reads it yet, so without this a
// silently-empty seam would only be discovered later.
func TestLastCaptureReturnsTheLinesOutput(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	// Drain the terminal side so the copy loop is never blocked writing,
	// keeping what it saw so a failure can be explained.
	var screen bytes.Buffer
	var screenMu sync.Mutex
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				screenMu.Lock()
				screen.Write(buf[:n])
				screenMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	// nil tty: the table then makes no foreground-group handoff, which a
	// test process cannot do anyway (it does not own this terminal).
	// Capture does not depend on it — only on the child's stdout being a
	// terminal, which the pty slave below is.
	table := jobs.NewTable(nil)
	table.EnableCapture(0)

	runner, err := interp.New(
		interp.StdIO(nil, tty, tty), // both must be *os.File or ExecMiddleware falls through
		interp.Env(expand.ListEnviron(os.Environ()...)),
		interp.Dir(t.TempDir()),
		interp.ExecHandlers(table.ExecMiddleware),
	)
	if err != nil {
		t.Fatal(err)
	}

	// An *external* command: builtins never reach ExecMiddleware, so
	// they are not captured — see the note below.
	const script = `/bin/echo captured-marker`
	table.BeginLine(script)
	file, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		t.Fatal(err)
	}
	runErr := runner.Run(context.Background(), file)
	table.EndLine()
	screenMu.Lock()
	seen := screen.String()
	screenMu.Unlock()
	if runErr != nil {
		t.Fatalf("run: %v (terminal saw: %q)", runErr, seen)
	}

	out, truncated := table.LastCapture()
	if !strings.Contains(string(out), "captured-marker") {
		t.Errorf("LastCapture = %q, want the command's output (terminal saw: %q)", out, seen)
	}
	if truncated {
		t.Error("a short line reported truncation")
	}
}
