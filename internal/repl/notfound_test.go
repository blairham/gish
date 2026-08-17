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
	// The suggestion is the whole line, not the word (#163): the user
	// typed arguments too, and a suggestion they have to retype the rest
	// of is a suggestion they will not use.
	if !strings.Contains(out.String(), `did you mean "mycommand arg"?`) {
		t.Errorf("no suggestion: %q", out.String())
	}
}

// A distro command-not-found hook must run, and must receive the whole
// command line (#163). Debian/Ubuntu and Fedora both ship one, both read
// "$@", and both are how a miss becomes "install package X" rather than
// a dead end.
func TestNotFoundHookGetsFullCommandLine(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	for _, name := range []string{"command_not_found_handle", "command_not_found_handler"} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			var runner *interp.Runner
			runner, err := interp.New(
				interp.StdIO(nil, &out, &out),
				interp.ExecHandlers(notFoundMiddleware(func() *interp.Runner { return runner })),
			)
			if err != nil {
				t.Fatal(err)
			}
			src := name + `() { echo "hook:$*"; return 42; }` + "\n" + `nosuchcmd --flag "two words"`
			rerr := runner.Run(t.Context(), parseLine(t, src))

			if got := out.String(); !strings.Contains(got, `hook:nosuchcmd --flag two words`) {
				t.Errorf("hook did not see the full line: %q", got)
			}
			if strings.Contains(out.String(), "command not found") {
				t.Errorf("gish spoke over the hook: %q", out.String())
			}
			// The hook's status is the command's status, as in bash.
			if status, ok := errors.AsType[interp.ExitStatus](rerr); !ok || status != 42 {
				t.Errorf("err = %v, want exit 42", rerr)
			}
		})
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
