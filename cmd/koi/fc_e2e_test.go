//go:build unix

package main

import (
	"strings"
	"testing"
)

// fc -l (#60), driven through a pty because history is a property of an
// interactive session: the -c path records nothing, so a script-mode
// test of fc would assert against an empty store and pass for the wrong
// reason.
func TestFCListsHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	// Three commands worth listing. Waiting for each command's *output*
	// is not enough to send the next line: the shell has still to finish
	// and re-enter raw mode, and a line typed into that window is
	// discarded. runProbe waits for the output and then for the command
	// to finish (#195's sibling).
	for _, name := range []string{"ONE", "TWO", "THREE"} {
		s.runProbe(`printf "res%s\n" `+name, "res"+name)
	}

	out := s.runProbe(`fc -l; printf "res%s\n" LISTED`, "resLISTED")
	for _, want := range []string{"ONE", "TWO", "THREE"} {
		if !strings.Contains(out, want) {
			t.Errorf("fc -l did not list the %s command:\n%s", want, out)
		}
	}

	// -n drops the numbers; the commands stay.
	s.runProbe(`fc -l -n 1; printf "res%s\n" NONUM`, "resNONUM")

	// The re-execute form runs a command again (#711). The entry is named
	// by *prefix* rather than left to a bare `fc -s`, because an
	// interactive session's history is the shared store (#40) and which
	// entry is newest there is a property of the store rather than of this
	// session — a prefix names the same line either way. The arithmetic
	// itself is covered differentially over a real file in
	// histresidue_test.go; what this asserts is that the form is reachable
	// from a terminal at all.
	s.runProbe(`: ALPHA; printf "res%s\n" ALPHA`, "resALPHA")
	rerun := s.runProbe(`fc -s ALPHA=BETA :; printf "res%s\n" RERUN`, "resRERUN")
	if !strings.Contains(rerun, "resBETA") {
		t.Errorf("fc -s did not re-run the command with its substitution applied:\n%s", rerun)
	}

	// The editor form still says what it does not do, rather than
	// answering "unsupported builtin" like a name nobody implemented.
	edit := s.runProbe(`fc -e nosucheditor; printf "res%s\n" EDIT`, "resEDIT")
	if !strings.Contains(edit, "editor form is not implemented") {
		t.Errorf("fc -e did not explain itself:\n%s", edit)
	}
}
