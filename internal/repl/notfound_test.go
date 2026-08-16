package repl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

func TestEditDistance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		want int
	}{
		{"git", "git", 0},
		// Deliberate typos: this is a typo-detector's test data.
		{"gti", "git", 1},   // adjacent transposition costs 1 (Damerau)
		{"mkae", "make", 1}, //nolint:misspell // the typo is the test
		{"sl", "ls", 1},
		{"completely", "different", 3}, // clamped at bound+1
	}
	for _, tt := range tests {
		if got := editDistance(tt.a, tt.b, 2); got != tt.want {
			t.Errorf("editDistance(%q,%q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestNotFoundSuggests(t *testing.T) {
	// A fake PATH with one executable to suggest.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, exeFixture("mycommand")), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // fake PATH entry
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	var out strings.Builder
	runner, err := interp.New(
		interp.StdIO(nil, &out, &out),
		interp.ExecHandlers(notFoundMiddleware(func() *interp.Runner { return nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Late-bind for the suggestion path.
	runner2, err := interp.New(
		interp.StdIO(nil, &out, &out),
		interp.ExecHandlers(notFoundMiddleware(func() *interp.Runner { return runner })),
	)
	if err != nil {
		t.Fatal(err)
	}

	rerr := runner2.Run(t.Context(), parseLine(t, "mycommnd arg"))
	if status, ok := errors.AsType[interp.ExitStatus](rerr); !ok || status != 127 {
		t.Fatalf("err = %v, want 127", rerr)
	}
	if !strings.Contains(out.String(), "command not found: mycommnd") {
		t.Errorf("stderr = %q", out.String())
	}
	if !strings.Contains(out.String(), `did you mean "mycommand"?`) {
		t.Errorf("no suggestion: %q", out.String())
	}
}

func TestNotFoundPathCommandsPassThrough(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	runner, err := interp.New(
		interp.StdIO(nil, &out, &out),
		interp.ExecHandlers(notFoundMiddleware(func() *interp.Runner { return nil })),
	)
	if err != nil {
		t.Fatal(err)
	}
	rerr := runner.Run(t.Context(), parseLine(t, "/definitely/not/here"))
	if rerr == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(out.String(), "did you mean") {
		t.Errorf("path commands must not get suggestions: %q", out.String())
	}
}
