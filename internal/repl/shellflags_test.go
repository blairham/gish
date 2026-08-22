package repl

import (
	"slices"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/jobs"
)

// sorted makes the comparison about *which* letters are claimed rather
// than their order: ordering `$-` is the interpreter's job, since it
// holds the other half of the string (interp.optionFlags).
func sorted(s string) string {
	letters := strings.Split(s, "")
	slices.Sort(letters)
	return strings.Join(letters, "")
}

// The invocation letter is the one part of `$-` that is decided before
// the shell runs anything, and bash reports all three states: `c` for
// -c, `s` for commands read from standard input, and neither for a
// script named on the command line. koi used to answer `s` on every
// path, including `-c`, where bash answers `c`.
func TestShellFlagsReportTheInvocation(t *testing.T) {
	tests := []struct {
		name string
		f    sessionFlags
		want string
	}{
		{"script file", sessionFlags{invocation: invokedScript}, "B"},
		{"-c", sessionFlags{invocation: invokedCommand}, "Bc"},
		{"piped stdin", sessionFlags{invocation: invokedStdin}, "Bs"},
		{
			"-ic sources the rc but runs no line editor",
			sessionFlags{interactive: true, invocation: invokedCommand},
			"Bci",
		},
		{
			// H is absent here on purpose: history expansion is an
			// option now, so the interpreter renders its letter (#559).
			"the interactive loop",
			sessionFlags{interactive: true, invocation: invokedStdin},
			"Bis",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sorted(shellFlags(tc.f)); got != sorted(tc.want) {
				t.Errorf("shellFlags = %q, want %q", got, tc.want)
			}
		})
	}
}

// `m` is claimed from two facts, not one: the path has to be running job
// control *and* the platform has to support it. Claiming it from either
// alone would put a letter in a variable that exists to be probed.
func TestJobControlLetterNeedsBothFacts(t *testing.T) {
	if strings.Contains(shellFlags(sessionFlags{interactive: true}), "m") {
		t.Error("m claimed on a path with no job control")
	}
	got := strings.Contains(shellFlags(sessionFlags{interactive: true, jobControl: true}), "m")
	if got != jobs.Supported() {
		t.Errorf("m claimed = %v, want %v (jobs.Supported)", got, jobs.Supported())
	}
}

// The letters the *interpreter* owns must never be claimed here: they
// change on any line, and a copy that went stale is exactly the bug
// (#265). Nothing in this half may answer for errexit and friends.
func TestShellFlagsNeverClaimOptionLetters(t *testing.T) {
	every := sessionFlags{interactive: true, jobControl: true, invocation: invokedStdin}
	for _, letter := range "aefuxCETnH" {
		if strings.ContainsRune(shellFlags(every), letter) {
			t.Errorf("%q is the interpreter's to report, and was claimed here", letter)
		}
	}
}
