// Copyright (c) 2026, Blair Hamilton. See LICENSE for licensing information.

package interp_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"
	"github.com/blairham/koi-shell/internal/shell/syntax"
)

// TestReadonlyAssignmentAbortsTheCommandNotTheShell pins what bash means by
// calling this error fatal (#308).
//
// It throws away the command it is running and goes back to reading input; it
// does not kill the shell, which it only does under `set -o posix`. koi marked
// the failure as `exiting`, so a script lost every line after the offending
// one -- the cleanup, the teardown trap, the rest of a test suite.
//
// The resuming point is the next *line* rather than the next statement,
// because a line is bash's reading unit. All of these were measured against
// bash 5.3 rather than reasoned about; the two "same line" cases are the ones
// that distinguish a line rule from a statement rule.
func TestReadonlyAssignmentAbortsTheCommandNotTheShell(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want string // stdout only; the diagnostic goes to stderr
		code uint8
	}{{
		name: "the next line still runs",
		src:  "readonly foo=one\nfoo=4\necho done\n",
		want: "done\n",
	}, {
		name: "the rest of the same line does not",
		src:  "readonly foo=one; foo=4; echo done\n",
		want: "",
		code: 1,
	}, {
		name: "a later statement on the aborted line does not",
		src:  "readonly foo=one\nfoo=4; echo same-line\necho next-line\n",
		want: "next-line\n",
	}, {
		name: "the whole function call is abandoned, and the script goes on",
		src:  "readonly foo=one\nf() { echo in1; foo=4; echo in2; }\nf\necho after\n",
		want: "in1\nafter\n",
	}, {
		name: "a loop is abandoned rather than continued",
		src:  "readonly foo=one\nfor i in 1 2; do echo \"i=$i\"; foo=4; done\necho after\n",
		want: "i=1\nafter\n",
	}, {
		// The reason the issue was filed: a teardown below the failure
		// never ran, which is the opposite of what a trap is for.
		name: "an EXIT trap set before the failure still fires",
		src:  "readonly foo=one\ntrap 'echo teardown' EXIT\nfoo=4\necho after\n",
		want: "after\nteardown\n",
	}, {
		// Unchanged behavior, kept here so the fix cannot quietly widen:
		// these spellings were never fatal.
		name: "a command prefix carries on, as before",
		src:  "readonly foo=one; foo=4 true; echo done\n",
		want: "done\n",
	}, {
		name: "declare carries on, as before",
		src:  "readonly foo=one; declare foo=4; echo done\n",
		want: "done\n",
	}, {
		name: "an abort inside a subshell ends only that subshell",
		src:  "readonly foo=one; ( foo=4; echo in-sub ); echo after\n",
		want: "after\n",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file, err := syntax.NewParser().Parse(strings.NewReader(tc.src), "")
			if err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			r, err := interp.New(interp.StdIO(strings.NewReader(""), &out, &errOut))
			if err != nil {
				t.Fatal(err)
			}
			runErr := r.Run(context.Background(), file)
			var code uint8
			var status interp.ExitStatus
			if runErr != nil {
				if !asExitStatus(runErr, &status) {
					t.Fatalf("unexpected error: %v", runErr)
				}
				code = uint8(status)
			}
			if out.String() != tc.want {
				t.Errorf("stdout: got %q, want %q (stderr %q)", out.String(), tc.want, errOut.String())
			}
			if code != tc.code {
				t.Errorf("exit status: got %d, want %d", code, tc.code)
			}
			// The diagnostic is the point of the error and must survive
			// the change from fatal to abort.
			if !strings.Contains(errOut.String(), "readonly variable") {
				t.Errorf("stderr lost the diagnostic: %q", errOut.String())
			}
		})
	}
}

func asExitStatus(err error, out *interp.ExitStatus) bool {
	return errors.As(err, out)
}
