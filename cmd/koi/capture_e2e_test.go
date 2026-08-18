//go:build unix

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The success bar for output capture (#99 stage 2), as docs/blocks.md
// states it: with capture on, programs must behave exactly as they do
// today.
//
// This exists because unit tests could not catch the bug it guards
// against. The capture session's own tests all passed while the feature
// was unusable — a pager under capture painted a screen, never entered
// raw mode, and swallowed the keystrokes meant for it. Only a real
// full-screen program through a real pty showed that *which* descriptors
// get substituted is what decides whether this works.
//
// The pty plumbing lives in ptyharness_test.go; everything here is the
// behavior being asserted.

// startCaptureShell is a session with capture already on. Setting it in
// the environment rather than typing `config blocks on` saves seventeen
// keystrokes — each one a full prompt redraw, ~0.3s on a loaded runner —
// and config_test already proves the command works.
func startCaptureShell(t *testing.T, dir string) *ptySession {
	t.Helper()
	// Few rows so a pager actually pages; the harness default width
	// keeps a long CI hostname from wrapping the prompt.
	return startPTY(t, ptyOptions{Dir: dir, Rows: 15, Env: []string{"KOI_BLOCKS=on"}})
}

// A pager must still be a pager: it takes over the screen, responds to
// its own keys, and hands the shell back on quit. This is the exact
// case that failed when stderr was captured too.
func TestCapturedPagerStillWorks(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	if _, err := exec.LookPath("less"); err != nil {
		t.Skip("less not installed")
	}
	dir := t.TempDir()
	var lines bytes.Buffer
	for i := range 200 {
		lines.WriteString(string(rune('a'+i%26)) + "-line\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), lines.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	s := startCaptureShell(t, dir)
	s.waitForPrompt()
	s.send("less big.txt\r")
	s.waitFor("big.txt") // the pager's status line

	// `q` is a raw keystroke, not a line: only a program actually in raw
	// mode acts on it, which is the whole assertion. Repeated because the
	// pager may not be listening the instant its status line lands.
	s.sendUntil("q", promptEnd)
	s.probe("PAGERDONE")
	s.waitFor("resPAGERDONE")
}

// git chooses to page by asking isatty(stdout). Under capture that must
// still be true — a pipe would make it dump, which is the behavior
// change that disqualified pipes in the first place.
func TestCapturedGitStillPages(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	for _, tool := range []string{"git", "less"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Skipf("git setup failed: %v", err)
		}
	}
	for i := range 40 {
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte{byte('0' + i%10)}, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "f"}, {"commit", "-qm", "commit-number-" + string(rune('a'+i%26))}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			if err := cmd.Run(); err != nil {
				t.Skipf("git commit failed: %v", err)
			}
		}
	}

	s := startCaptureShell(t, dir)
	s.waitForPrompt()
	s.send("git log --oneline\r")
	s.waitFor("commit-number") // paged output on screen
	s.sendUntil("q", promptEnd)
	s.probe("GITDONE")
	s.waitFor("resGITDONE")
}

// Capture must not change what a redirect writes. Routing `cmd > file`
// through the pty would translate newlines and silently rewrite the
// file's bytes.
//
// The result is read from disk rather than from the screen on purpose:
// the terminal echoes the command as it is typed, so any sentinel
// chosen for the output can also appear in the echo. Reading the file
// removes the ambiguity instead of trying to out-clever it.
func TestCaptureLeavesRedirectionAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	dir := t.TempDir()
	s := startCaptureShell(t, dir)
	s.waitForPrompt()
	// One line with its own marker: sending the command and probing as a
	// second line races, because two writes back-to-back can lose the
	// second in the raw-mode transition around running the first.
	s.send(`printf 'hello\n' > out.txt; printf "res%s\n" WROTE` + "\r")
	s.waitFor("resWROTE")

	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\n" {
		t.Errorf("redirected file = %q, want %q — the pty rewrote it", got, "hello\n")
	}
}

// TestProbeSurvivesStrayInput pins the harness invariant that makes the
// pager tests trustworthy.
//
// sendUntil retries on silence, and its retry cannot be idempotent for
// every key: `q` is harmless while a pager is up and a literal character
// the instant it is not. A retry landing just after less exits leaves a
// `q` on the line, and the next command becomes `qprintf …` — so the
// probe waits sixty seconds for output that a command-not-found will
// never produce. That is the shape of the CI flake this guards.
//
// A stray keystroke is exactly reproducible even though the race is not,
// so the invariant gets a deterministic test rather than a rerun.
func TestProbeSurvivesStrayInput(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()
	s.send("q") // the stray character a sendUntil retry would leave
	s.waitFor("q")
	s.probe("STRAY")
	s.waitFor("resSTRAY")
}
