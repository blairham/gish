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

// The theme wizard's huh frontend (#90) can only be verified on a real
// terminal: the unit tests drive the line frontend, and the whole point
// of the form is what it does with a pty. This is also the standing
// guard for the class of bug that disqualified bubbletea v1 — a charm
// package that queries the terminal on start hangs here, in CI, rather
// than in someone's shell.

// wizardSession drives one `gish -c config theme` on a pty.
type wizardSession struct {
	t   *testing.T
	f   *os.File
	buf *bytes.Buffer
	ch  <-chan []byte
}

func startWizard(t *testing.T, extraEnv ...string) *wizardSession {
	t.Helper()
	bin := buildGish(t)
	base := t.TempDir()

	cmd := exec.Command(bin, "-c", "config theme")
	cmd.Env = append([]string{
		"HOME=" + base,
		"XDG_CONFIG_HOME=" + filepath.Join(base, "config"),
		"XDG_DATA_HOME=" + filepath.Join(base, "data"),
		"TERM=xterm-256color",
		"PATH=" + os.Getenv("PATH"),
	}, extraEnv...)
	cmd.Dir = base

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 200})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Reader goroutine plus select: a pty read blocks past any deadline
	// once output goes quiet, so the timeout cannot live in the Read.
	ch := make(chan []byte, 64)
	go func() {
		defer close(ch)
		for {
			chunk := make([]byte, 4096)
			n, err := f.Read(chunk)
			if n > 0 {
				ch <- chunk[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	return &wizardSession{t: t, f: f, buf: &bytes.Buffer{}, ch: ch}
}

// waitFor blocks until want appears in the ANSI-stripped output.
func (s *wizardSession) waitFor(want string) string {
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
				s.t.Fatalf("pty closed before %q; got:\n%s", want, plain)
			}
			s.buf.Write(chunk)
		case <-deadline:
			s.t.Fatalf("did not see %q within 20s; got:\n%s", want, plain)
		}
	}
}

// step waits for a field to be *live*, then sends keys.
//
// Waiting for the title is not enough. huh enters raw mode when the
// form starts, and switching a terminal to raw mode discards whatever
// is already queued — so a key written between the title appearing and
// the form reading is simply lost. `ready` is a string that only
// renders once the field is drawing and accepting input (its help bar),
// which is the earliest safe moment to type.
//
// The buffer is cleared after each match so the next wait cannot be
// satisfied by output from a previous field.
func (s *wizardSession) step(ready, keys string) {
	s.t.Helper()
	s.waitFor(ready)
	s.buf.Reset()
	if _, err := s.f.WriteString(keys); err != nil {
		s.t.Fatal(err)
	}
}

// help-bar fragments that mean "this field is live and reading keys".
const (
	selectLive  = "filter" // selects offer / to filter
	confirmLive = "toggle" // confirms offer ←/→
)

// The short path: pick a theme, confirm, and the answer is written
// through the same persistence the command line uses.
func TestThemeWizardFormSaves(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startWizard(t)

	s.waitFor("theme configurator")
	s.step(selectLive, "\r") // take the focused option (plain)
	s.step(confirmLive, "\r")

	// GISH_THEME was unset, so choosing plain *is* a change and lands in
	// the rc file — the wizard writes only what actually differs.
	out := s.waitFor("saved to")
	if !bytes.Contains([]byte(out), []byte("gishrc")) {
		t.Errorf("did not name the rc file:\n%s", out)
	}
}

// Choosing a value that is already set writes nothing: the wizard
// persists differences, not answers.
func TestThemeWizardSavesNothingWhenUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startWizard(t, "GISH_THEME=plain")

	s.waitFor("theme configurator")
	s.step(selectLive, "\r")
	s.step(confirmLive, "\r")
	s.waitFor("nothing changed")
}

// The full p10k path — separator, layout, frame, segments, preview.
// This is the form's real workload and the one that exercises every
// field type the seam offers.
func TestThemeWizardFormWalksP10kPath(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startWizard(t)

	s.waitFor("theme configurator")
	s.step(selectLive, "\x1b[B\r") // down to p10k, enter

	s.waitFor("separator preview")
	s.step(selectLive, "\r") // keep plain separators
	s.step(selectLive, "\r") // layout: keep 2
	s.step(selectLive, "\r") // frame: keep on

	// The segment list is a free-text field, so its help bar differs;
	// wait for the question itself, which only renders now.
	s.step("segments, in order", "\r")

	s.waitFor("preview:")
	s.step(confirmLive, "\r")
	s.waitFor("saved to")
}

// NO_COLOR on a real terminal takes the line frontend, not a
// half-styled form — the degradation rule every other surface follows.
func TestThemeWizardFallsBackUnderNoColor(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startWizard(t, "NO_COLOR=1")

	// The line frontend echoes the default in parentheses; the form
	// never does. That is the observable difference between them.
	out := s.waitFor("theme (plain): ")
	if bytes.ContainsRune([]byte(out), 0x1b) {
		t.Errorf("escape bytes under NO_COLOR:\n%q", out)
	}

	s.buf.Reset()
	if _, err := s.f.WriteString("plain\n"); err != nil {
		t.Fatal(err)
	}
	s.waitFor("save? (y): ")
	if _, err := s.f.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}
	s.waitFor("saved to")
}

// Aborting saves nothing: the rc file is untouched until the final
// confirmation, whichever frontend asked.
func TestThemeWizardAbortSavesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startWizard(t)

	s.waitFor("theme configurator")
	s.step(selectLive, "\x03") // Ctrl-C

	out := s.waitFor("nothing saved")
	if bytes.Contains([]byte(out), []byte("saved to")) {
		t.Errorf("abort wrote the rc file:\n%s", out)
	}
}
