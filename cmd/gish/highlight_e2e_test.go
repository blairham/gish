//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Highlight verdicts asserted from the raw pty bytes (#193).
//
// Every other e2e strips ANSI before matching, which is how "valid
// command renders red" shipped: the highlighter's private known-list
// missed aliases and most of gish's own commands, and no test had ever
// looked at a color. The waits here run against the unstripped buffer —
// seen() checks raw bytes first — so the assertion is the escape the
// user's terminal actually paints.
func TestHighlightPaintsValidCommandsGreen(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	const (
		green = "\x1b[32m"
		red   = "\x1b[31m"
	)
	dir := t.TempDir()
	rc := filepath.Join(dir, "gishrc")
	if err := os.WriteFile(rc, []byte("alias gg='git status'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := startPTY(t, ptyOptions{Dir: dir, Env: []string{"GISH_RC=" + rc}})
	s.waitForPrompt()

	// A PATH executable: the case that always worked.
	s.send("ls")
	s.waitFor(green + "ls")
	s.send("\x15") // ctrl-u: clear the line between checks

	// An alias from the rc: red before #193, because the interpreter's
	// alias map is unexported and nothing mirrored it.
	s.send("gg")
	s.waitFor(green + "gg")
	s.send("\x15")

	// A CallHandler-routed gish command: red before #193, because the
	// highlighter kept a private `zi, config` pair instead of reading
	// callHandlerCommands.
	s.send("doctor")
	s.waitFor(green + "doctor")
	s.send("\x15")

	// An alias defined at the prompt, not in the rc: proves the live
	// mirror, not just startup sourcing. The marker probes completion of
	// the definition; waiting on the prompt would match the one already
	// in the buffer.
	s.send("alias vv='ls'; printf 'res%s\\n' DEFINED\r")
	s.waitFor("resDEFINED")
	s.send("vv")
	s.waitFor(green + "vv")
	s.send("\x15")

	// And an honest typo must still be called one.
	s.send("zzqqx")
	s.waitFor(red + "zzqqx")
}
