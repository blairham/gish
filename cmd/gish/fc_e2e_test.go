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

	// Three commands worth listing, each waited for so the history entry
	// is written before the next line is typed.
	for _, name := range []string{"ONE", "TWO", "THREE"} {
		s.buf.Reset()
		s.send(`printf "res%s\n" ` + name + "\r")
		s.waitFor("res" + name)
	}

	s.buf.Reset()
	s.send(`fc -l; printf "res%s\n" LISTED` + "\r")
	s.waitFor("resLISTED")
	out := s.plain()
	for _, want := range []string{"ONE", "TWO", "THREE"} {
		if !strings.Contains(out, want) {
			t.Errorf("fc -l did not list the %s command:\n%s", want, out)
		}
	}

	// -n drops the numbers; the commands stay.
	s.buf.Reset()
	s.send(`fc -l -n 1; printf "res%s\n" NONUM` + "\r")
	s.waitFor("resNONUM")

	// The editing forms say what they do not do, rather than answering
	// "unsupported builtin" like a name nobody implemented.
	s.buf.Reset()
	s.send(`fc -s; printf "res%s\n" EDIT` + "\r")
	s.waitFor("resEDIT")
	if !strings.Contains(s.plain(), "only the listing form") {
		t.Errorf("fc -s did not explain itself:\n%s", s.plain())
	}
}
