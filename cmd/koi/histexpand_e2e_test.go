//go:build unix

package main

import (
	"strings"
	"testing"
)

// The interactive half of #559, at a live prompt.
//
// History expansion at the prompt is #96's and has worked all along; what
// changed is that it is now an *option* the interpreter holds, so this is
// the guard on the seam rather than a new feature. Two things have to be
// true at once and were not: `set -o` must agree with what the shell
// does, and `set +H` — a line people type — must actually turn `!!` off,
// where koi used to accept it and carry on expanding.
//
// pty discipline (#240): the buffer is cleared before a send and the wait
// is on the OSC 133 D mark, never on the prompt. The off-case asserts a
// *diagnostic* rather than the absence of an expansion, because dropped
// keystrokes also produce no expansion — a missing thing needs evidence
// the input arrived.
func TestSetHMovesHistoryExpansionAtThePrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	// The state and the letter, asked for as a probe prints both answers
	// whichever way they fall: a marker that is only the right answer
	// fails by waiting for silence rather than by saying what it saw.
	askH := func(line string) string {
		t.Helper()
		probe := line + `; printf 'resH=%s,%s\n' "$(set -o | grep -E '^histexpand ' | awk '{print $2}')" ` +
			`"$(case $- in *H*) echo letter;; *) echo none;; esac)"`
		return commandOutput(s.runProbe(probe, "resH="))
	}

	if got := askH("true"); !strings.Contains(got, "resH=on,letter") {
		t.Errorf("a shell with a line editor says %q, want histexpand on and H in $-", got)
	}

	s.runLine("echo hist-alpha")
	s.buf.Reset()
	s.send("!!\r")
	s.waitFor(commandDone)
	if got := commandOutput(s.plain()); !strings.Contains(got, "hist-alpha") {
		t.Errorf("`!!` printed %q, want the previous command's output:\n%s", got, s.plain())
	}

	if got := askH("set +H"); !strings.Contains(got, "resH=off,none") {
		t.Errorf("after `set +H` the shell says %q, want histexpand off and no H in $-", got)
	}
	s.buf.Reset()
	s.send("!!\r")
	s.waitFor(commandDone)
	// `!!` is now an ordinary word, so it reaches the exec seam and says
	// so. That diagnostic is the evidence the line arrived at all.
	if got := s.plain(); !strings.Contains(got, "command not found") {
		t.Errorf("after `set +H`, `!!` did not reach the exec seam:\n%s", got)
	}

	if got := askH("set -H"); !strings.Contains(got, "resH=on,letter") {
		t.Errorf("after `set -H` the shell says %q, want histexpand on again", got)
	}
	s.runLine("echo hist-beta")
	s.buf.Reset()
	s.send("!!\r")
	s.waitFor(commandDone)
	if got := commandOutput(s.plain()); !strings.Contains(got, "hist-beta") {
		t.Errorf("`!!` printed %q after being turned back on, want the previous output:\n%s", got, s.plain())
	}
}
