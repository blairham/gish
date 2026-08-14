package repl_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/swash/internal/repl"
)

func TestRunReaderEcho(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	err := repl.RunReader(context.Background(),
		strings.NewReader(`greeting=hello; echo "$greeting world"`), "test",
		interp.StdIO(nil, &out, io.Discard))
	if err != nil {
		t.Fatalf("RunReader: %v", err)
	}
	if got, want := out.String(), "hello world\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestRunReaderExitStatus(t *testing.T) {
	t.Parallel()

	err := repl.RunReader(context.Background(),
		strings.NewReader("exit 42"), "test",
		interp.StdIO(nil, io.Discard, io.Discard))
	status, ok := errors.AsType[interp.ExitStatus](err)
	if !ok {
		t.Fatalf("err = %v, want exit status", err)
	}
	if status != 42 {
		t.Errorf("status = %d, want 42", status)
	}
}

func TestRunReaderParseError(t *testing.T) {
	t.Parallel()

	err := repl.RunReader(context.Background(),
		strings.NewReader("if then fi"), "test",
		interp.StdIO(nil, io.Discard, io.Discard))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}
