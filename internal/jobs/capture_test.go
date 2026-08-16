//go:build unix

package jobs_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/jobs"
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
