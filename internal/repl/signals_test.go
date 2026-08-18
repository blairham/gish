package repl

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

func parseLine(t *testing.T, src string) *syntax.File {
	t.Helper()
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "test")
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func newTestRunner(t *testing.T) *interp.Runner {
	t.Helper()
	runner, err := interp.New(interp.StdIO(nil, io.Discard, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestRunInterruptibleStopsBuiltinLoop(t *testing.T) {
	t.Parallel()

	runner := newTestRunner(t)
	sigs := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		// A pure-builtin infinite loop: no external process exists for
		// the kernel to signal — only context cancellation can stop it.
		release, err := runInterruptible(context.Background(), runner, parseLine(t, "while true; do true; done"), sigs)
		release()
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	sigs <- os.Interrupt

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted loop returned nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not stop the builtin loop")
	}

	// The runner must remain usable for the next prompt.
	if err := runner.Run(context.Background(), parseLine(t, "echo alive")); err != nil {
		t.Fatalf("runner unusable after interrupt: %v", err)
	}
}

func TestRunInterruptibleIgnoresSigquit(t *testing.T) {
	t.Parallel()

	runner := newTestRunner(t)
	sigs := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		release, err := runInterruptible(context.Background(), runner, parseLine(t, "while true; do true; done"), sigs)
		release()
		done <- err
	}()

	// SIGQUIT must not cancel the command from the shell's side.
	sigs <- syscall.SIGQUIT
	select {
	case err := <-done:
		t.Fatalf("SIGQUIT canceled the command: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	sigs <- os.Interrupt
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not stop the loop after SIGQUIT")
	}
}

func TestRunInterruptibleCompletesNormally(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	runner, err := interp.New(interp.StdIO(nil, &out, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	sigs := make(chan os.Signal, 1)
	release, err := runInterruptible(context.Background(), runner, parseLine(t, "echo done"), sigs)
	release()
	if err != nil {
		t.Fatalf("runInterruptible: %v", err)
	}
	if out.String() != "done\n" {
		t.Errorf("output = %q", out.String())
	}
}

func TestDrainSignals(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal, 1)
	sigs <- os.Interrupt
	drainSignals(sigs)
	select {
	case s := <-sigs:
		t.Fatalf("signal %v not drained", s)
	default:
	}
	drainSignals(sigs) // draining an empty channel must not block
}

func TestInterruptedCommandReturnsSentinel(t *testing.T) {
	t.Parallel()

	// An interrupted run must surface as errInterrupted so runEditor can
	// continue silently — and it must be checked before Runner.Exited(),
	// which also reports true after a cancellation.
	runner := newTestRunner(t)
	sigs := make(chan os.Signal, 1)
	sigs <- os.Interrupt // canceled immediately
	release, err := runInterruptible(context.Background(), runner, parseLine(t, "while true; do true; done"), sigs)
	defer release()
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v, want errInterrupted", err)
	}
	if !runner.Exited() {
		t.Log("note: Runner.Exited() no longer reports true after cancellation")
	}
}
