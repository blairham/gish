//go:build unix

package main

import (
	"strings"
	"testing"
)

// `set -o vi` typed at a live prompt has to move the editor *and* the
// option, and `set -o emacs` has to move both back (#576).
//
// TestViModeFromRC already drives the rc spelling into the editor. What
// is new here is the pair, in one session: the option is read back
// through the shell after each switch, and each mode is then proven by a
// keystroke sequence that is meaningless in the other one — `ciw` needs a
// vi normal mode to exist, and Alt-f/Alt-u need an emacs keymap, since in
// vi mode Escape and Alt are the same byte and resolve the other way.
//
// The return trip is the half that had no coverage at all: #163's rewrite
// could switch a shell into vi mode and there was no test that anything
// could switch it out.
func TestSetOEditModeMovesTheEditorAndTheOption(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	// Both rows are printed rather than only the one that is on, so the
	// marker appears whatever the answer: a probe whose marker is the
	// *right* answer fails by waiting for silence instead of by saying
	// what it saw. "resmode=e" is not in the echo of the typed line,
	// which contains the format string and not its result.
	askMode := func(mode string) string {
		t.Helper()
		line := "set -o " + mode +
			`; printf 'resmode=%s\n' "$(set -o | grep -E '^(emacs|vi) ' | awk '{printf "%s:%s;", $1, $2}')"`
		return commandOutput(s.runProbe(line, "resmode=e"))
	}

	if got := askMode("vi"); !strings.Contains(got, "resmode=emacs:off;vi:on;") {
		t.Errorf("after `set -o vi` the options say %q, want vi on and emacs off", got)
	}
	// Type it wrong and fix the middle word with ciw — keystrokes with no
	// meaning in emacs mode, so the edit landing proves the switch.
	s.buf.Reset()
	s.send("echo WRONG tail")
	s.send("\x1b")                // Escape to normal mode
	s.send("bbciwvi-typed-works") // back two words, change inner word
	s.send("\r")
	s.waitFor(commandDone)
	// What the command printed, never the buffer: the editor redraws the
	// line on every edit, so the whole-buffer version of this assertion
	// is a redraw detector (#240).
	if got := commandOutput(s.plain()); !strings.Contains(got, "vi-typed-works tail") {
		t.Errorf("vi keys printed %q, want the edited line — the editor did not switch:\n%s", got, s.plain())
	}

	if got := askMode("emacs"); !strings.Contains(got, "resmode=emacs:on;vi:off;") {
		t.Errorf("after `set -o emacs` the options say %q, want emacs on and vi off", got)
	}
	// And back: Alt-f then Alt-u, which in vi mode would be Escape and
	// two normal-mode commands rather than upcase-word.
	s.buf.Reset()
	s.send("echo hello\x01\x1bf\x1bu\r")
	s.waitFor(commandDone)
	if got := commandOutput(s.plain()); !strings.Contains(got, "HELLO") {
		t.Errorf("emacs keys printed %q, want HELLO — the editor stayed in vi mode:\n%s", got, s.plain())
	}
}
