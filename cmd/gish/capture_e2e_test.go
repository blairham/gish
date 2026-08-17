//go:build unix

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The success bar for output capture (#99 stage 2), as docs/blocks.md
// states it: with capture on, programs must behave exactly as they do
// today.
//
// This exists because unit tests could not catch the bug it is guarding
// against. The capture session's own tests all passed while the feature
// was unusable — a pager under capture painted a screen, never entered
// raw mode, and swallowed the keystrokes meant for it. Only a real
// full-screen program through a real pty showed that *which*
// descriptors get substituted is what decides whether this works.

// promptReady is the naked prompt's tail — gish starts naked, so the
// prompt ends in "% " rather than a shell-conventional "$ ".
const promptReady = " % "

type shellPTY struct {
	t   *testing.T
	f   *os.File
	buf *bytes.Buffer
	ch  <-chan []byte
}

func startShell(t *testing.T, dir string) *shellPTY {
	t.Helper()
	bin := buildGish(t)
	base := t.TempDir()
	if dir == "" {
		dir = base
	}
	cmd := exec.Command(bin)
	cmd.Env = []string{
		"HOME=" + base,
		"XDG_CONFIG_HOME=" + filepath.Join(base, "config"),
		"XDG_DATA_HOME=" + filepath.Join(base, "data"),
		"XDG_STATE_HOME=" + filepath.Join(base, "state"),
		"TERM=xterm-256color",
		"PATH=" + os.Getenv("PATH"),
	}
	cmd.Dir = dir
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 15, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	ch := make(chan []byte, 64)
	go func() {
		defer close(ch)
		for {
			chunk := make([]byte, 8192)
			n, err := f.Read(chunk)
			if n > 0 {
				ch <- chunk[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	return &shellPTY{t: t, f: f, buf: &bytes.Buffer{}, ch: ch}
}

func (s *shellPTY) waitFor(want string) string {
	s.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		plain := string(ansiRe.ReplaceAll(s.buf.Bytes(), nil))
		if bytes.Contains([]byte(plain), []byte(want)) {
			return plain
		}
		select {
		case chunk, ok := <-s.ch:
			if !ok {
				s.t.Fatalf("shell exited before %q; got:\n%s", want, plain)
			}
			s.buf.Write(chunk)
		case <-deadline:
			s.t.Fatalf("did not see %q within 20s; got:\n%s", want, plain)
		}
	}
}

func (s *shellPTY) send(keys string) {
	s.t.Helper()
	if _, err := s.f.WriteString(keys); err != nil {
		s.t.Fatal(err)
	}
}

// probe runs a command whose output cannot be confused with the echo of
// the command itself — the "res" prefix only ever appears in output.
func (s *shellPTY) probe(name string) { s.send("printf 'res%s\\n' " + name + "\r") }

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

	s := startShell(t, dir)
	s.waitFor(promptReady)
	s.send("config blocks on\r")
	s.waitFor("blocks")

	s.send("less big.txt\r")
	s.waitFor("big.txt") // the pager's status line
	s.send("q")          // a raw keystroke, not a line: only a pager in
	// raw mode acts on it, which is the whole assertion
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

	s := startShell(t, dir)
	s.waitFor(promptReady)
	s.send("config blocks on\r")
	s.waitFor("blocks")

	s.send("git log --oneline\r")
	s.waitFor("commit-number") // paged output on screen
	s.send("q")
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
	s := startShell(t, dir)
	s.waitFor(promptReady)
	s.send("config blocks on\r")
	s.waitFor("blocks")

	s.send("printf 'hello\\n' > out.txt\r")
	s.probe("WROTE")
	s.waitFor("resWROTE")

	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\n" {
		t.Errorf("redirected file = %q, want %q — the pty rewrote it", got, "hello\n")
	}
}
