//go:build unix

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The day-one papercut suite (#163).
//
// Every case here is a documented, individually sufficient reason
// someone abandoned a shell — not a hypothesis about what might annoy
// people. They are cheap to hold; the cost of losing one is a user who
// never comes back, and the only way to hold them is to drive the real
// interactive shell, because every one of them lives on the interactive
// path that unit tests do not reach.

// `sudo !!` is the single most-named muscle memory in the corpus. The
// unit tests cover the expansion; this covers the thing people actually
// type, at a real prompt, where the history store, the line editor and
// the expander all have to agree.
//
// It runs `env` rather than `sudo` for the obvious reason: a test must
// not need a password, and what is being verified is the expansion of a
// command *prefixed* onto the previous line, which is the same shape.
func TestBangBangWithACommandPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()
	// The quotes are load-bearing: the pty echoes every keystroke, so a
	// marker that appears in the typed text cannot prove the command
	// ran. Quoted on the way in, unquoted on the way out.
	s.send("echo pap'ercut-one'\r")
	s.waitFor("papercut-one")
	// Forget what has been seen, or the first command's own output
	// satisfies the wait below and the expansion is never checked at all.
	s.buf.Reset()
	s.send("env !!\r")
	// The expansion is echoed bash-style before it runs, and then it runs.
	out := s.waitFor("papercut-one")
	if !strings.Contains(out, "env echo pap") {
		t.Errorf("expansion was not echoed: %q", out)
	}
}

// Ctrl-Z. A user left a shell after six months over exactly this:
// "Then I tried C-z, it wasn't supported. Went back to zsh."
//
// The corner is the whole round trip — suspend a foreground command,
// see it in `jobs`, resume it with `fg`, and have the shell itself
// survive all of it.
func TestCtrlZSuspendsAndFgResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	// A command that runs long enough to be suspended, and that says so
	// when it resumes — "fg printed the job name" is not evidence the
	// process is running again.
	s.send("sh -c 'sleep 1; echo resumed-and-finished'\r")
	s.sendUntil("\x1a", "Stopped") // Ctrl-Z
	s.waitForPrompt()

	s.send("jobs\r")
	out := s.waitFor("Stopped")
	if !strings.Contains(out, "sleep 1") {
		t.Errorf("jobs did not list the stopped command: %q", out)
	}

	s.send("fg\r")
	s.waitFor("resumed-and-finished")

	// The shell is still ours afterwards.
	s.send("echo still-here\r")
	s.waitFor("still-here")
}

// An unquoted URL must not become a glob error.
//
// `mpv https://…?v=X` is a day-one quit in both fish and unconfigured
// zsh: the ? and the [] in a URL are pattern metacharacters, and a shell
// that errors on a failed match refuses to run the command at all. bash
// leaves an unmatched pattern alone, and so must we.
func TestUnquotedURLIsNotAGlobError(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()
	// Assigned first, echoed second, so what comes back is output rather
	// than the pty's echo of what was typed. The & is left out on
	// purpose: unquoted it backgrounds the command in bash too, so it is
	// not a papercut — the ? and the [] are, because a shell that errors
	// on an unmatched pattern refuses to run the command at all.
	s.send("u=https://example.com/watch?v=dQw4w9WgXcQ[a-b]\r")
	s.waitForPrompt()
	s.send(`echo "OUT:$u"` + "\r")
	out := s.waitFor("OUT:https://example.com/watch?v=dQw4w9WgXcQ")
	for _, bad := range []string{"no matches", "No matches", "no match found"} {
		if strings.Contains(out, bad) {
			t.Errorf("glob error on a URL: %q", out)
		}
	}
	if !strings.Contains(out, "OUT:https://example.com/watch?v=dQw4w9WgXcQ[a-b]") {
		t.Errorf("the unmatched pattern was not left alone: %q", out)
	}
}

// Config hygiene: two Reddit churn accounts died on a shell that
// scattered files. A shell that has been *used* may write what the user
// asked for; a shell that has merely been *started* must leave nothing
// behind, and nothing may ever land loose in $HOME.
func TestStartupWritesNothingToHome(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	home := t.TempDir()
	s := startPTY(t, ptyOptions{Env: []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
	}})
	s.waitForPrompt()
	s.send("exit\r")

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	// The XDG roots are where the harness pointed us; anything else is
	// clutter. Note that even these must not be *created* by startup —
	// checked below by name, since the harness names them explicitly.
	allowed := []string{"config", "data", "state", "cache"}
	for _, e := range entries {
		if slices.Contains(allowed, e.Name()) {
			continue
		}
		t.Errorf("startup left %q in $HOME", e.Name())
	}

	// And starting a shell must not pre-create the XDG trees either: a
	// directory is created when there is something to put in it.
	for _, dir := range allowed {
		if _, err := os.Stat(filepath.Join(home, dir)); err == nil {
			t.Errorf("startup created %s/ before anything needed it", dir)
		}
	}
}

// vi mode, end to end from the line people actually write.
//
// `set -o vi` in an rc is how a vi user arrives, and until now the
// interpreter answered `set: invalid option: "vi"` — the alias trap
// again, where the shell reports a broken rc while the setting quietly
// does not exist. This drives the real editor: Escape, a text object,
// and an insert, which is the composition that "the vim emulator sucks"
// is usually about.
func TestViModeFromRC(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	dir := t.TempDir()
	rc := filepath.Join(dir, "gishrc")
	if err := os.WriteFile(rc, []byte("set -o vi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := startPTY(t, ptyOptions{Dir: dir, Env: []string{"GISH_RC=" + rc}})
	s.waitForPrompt()

	// Type it wrong, then fix the middle word with ciw — the keystrokes
	// are meaningless in emacs mode, so passing proves the mode switch.
	s.send("echo WRONG tail")
	s.send("\x1b")              // Escape to normal mode
	s.send("bbciwvimode-works") // back two words, change inner word
	s.send("\r")
	out := s.waitFor("vimode-works tail")
	if strings.Contains(out, "invalid option") {
		t.Errorf("set -o vi was refused: %q", out)
	}
}
