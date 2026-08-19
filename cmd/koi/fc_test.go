//go:build unix

package main

import "testing"

// fc, differentially (#306).
//
// The three things this covers were three separate ways of disagreeing
// with something: `fc -l` disagreed with `history` about what was in the
// list, with bash about how to print it, and with bash about whether an
// empty list is an error.
func TestFCListingMatchesBash(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	// `history -s` puts commands in the list without running them, which
	// is the only way to get a known history into a non-interactive shell.
	const seed = `history -s one; history -s two; history -s three; `

	cases := []struct {
		name   string
		script string
	}{
		// The format. bash's `fc -l` is number, tab, space, command --
		// not `history`'s five-wide column, which is what koi printed.
		{"numbered", seed + `fc -l`},
		{"-n omits the numbers", seed + `fc -ln`},
		{"-n spelled separately", seed + `fc -l -n`},
		{"-r reverses", seed + `fc -lr`},

		// Ranges, including the negative first operand. `fc -l -2` is
		// "the last two" and was being read as an option named 2.
		{"an explicit range", seed + `fc -l 1 2`},
		{"from one to the end", seed + `fc -l 2`},
		{"counting back from the newest", seed + `fc -l -2`},
		{"the newest alone", seed + `fc -l -1`},
		{"a range counting back", seed + `fc -l -3 -1`},
		{"counting back, unnumbered", seed + `fc -ln -2`},

		// An empty list is not an error. A fresh shell listing its
		// history defensively should not end a script under `set -e`.
		{"empty history prints nothing", `fc -l; echo "rc=$?"`},
		{"empty history with a range", `fc -l 1; echo "rc=$?"`},

		// And the reconciliation itself: whatever `history` says is in
		// the list, `fc` has to be listing the same thing -- including
		// after the list has been edited underneath both of them.
		{"fc agrees with history", seed + `history; fc -l`},
		{"after history -c", seed + `history -c; fc -l; echo "rc=$?"`},
		{"after history -d", seed + `history -d 1; fc -l`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compareStdout(t, bashBin, koiBin, tc.script)
		})
	}
}
