//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The last mile for the themed prompt and ghost text, asserted as the
// bytes a terminal receives (#201).
//
// Every other e2e calls s.plain(), which strips escapes before matching,
// and no pty test waits on prompt characters — waitForPrompt waits on
// the zero-width OSC 133 mark by design. The result was that a themed
// prompt rendering entirely colorless, or ghost text never appearing,
// would break no test in the repo. seen() checks raw bytes before the
// stripped copy, so waiting on an escape works and costs nothing.
func TestThemedPromptAndGhostTextRenderStyled(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	dir := t.TempDir()
	rc := filepath.Join(dir, "koirc")
	// The koi theme with a right prompt: both halves are set by the
	// theme and rendered by the editor, which is the join no unit test
	// on either side can reach.
	if err := os.WriteFile(rc, []byte("KOI_THEME=koi\nKOI_THEME_RPROMPT=time\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := startPTY(t, ptyOptions{Dir: dir, Env: []string{"KOI_RC=" + rc}})
	s.waitForPrompt()

	// The themed prompt paints its directory segment. A theme that
	// resolved but produced no color would pass every existing test.
	s.waitFor("\x1b[")

	// Run a command so history has something to suggest, then type a
	// prefix of it and expect dim ghost text for the remainder.
	// runLine rather than send-then-waitForPrompt (#240): waiting for a
	// prompt after sending a line returns while the line is still being
	// echoed, because the editor redraws the whole prompt on every
	// keystroke, so the 133;B mark appears mid-line. The keystrokes that
	// follow then land in the raw-mode re-entry window and are discarded.
	// runLine waits for the 133;D mark, which appears only once the
	// command has actually finished.
	s.runLine("echo ghost-source")

	s.send("echo ghost-sou")
	// The editor wraps the *remainder* in the dim style, so match the
	// remainder with it: a bare \x1b[2m would also match the theme's own
	// dim frame and the highlighter's comment style, and would pass with
	// no ghost text on screen at all.
	const ghost = "\x1b[2mrce"
	s.waitFor(ghost)
	// And the knob really turns it off — same typing, no dim remainder.
	//
	// The leading Ctrl-U is part of the same write rather than a separate
	// send: two back-to-back writes can lose the second in a raw-mode
	// transition, and this was that pair. probe() leads with the same
	// character for the same reason.
	s.runLine("\x15KOI_SUGGEST=off")
	s.buf.Reset()
	s.send("echo ghost-sou")
	// Proof the line was actually typed, before asserting something is
	// *absent* from it. Without this the check below passes vacuously
	// whenever the keystrokes are dropped: no typing means no ghost text,
	// which is indistinguishable from the knob working.
	s.waitFor("ghost-sou")
	s.probe("SUGGESTOFF")
	out := s.waitFor("resSUGGESTOFF")
	if s.seen(ghost) {
		t.Errorf("KOI_SUGGEST=off still rendered ghost text; output:\n%s", out)
	}
}
