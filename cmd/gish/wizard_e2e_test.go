//go:build unix

package main

import (
	"bytes"
	"testing"
)

// The theme wizard's huh frontend (#90) can only be verified on a real
// terminal: the unit tests drive the line frontend, and the whole point
// of the form is what it does with a pty. This is also the standing
// guard against the bug class that disqualified bubbletea v1 — a charm
// package that queries the terminal on start hangs in CI rather than in
// someone's shell.
//
// The pty plumbing lives in ptyharness_test.go.

// startWizard runs `gish -c config theme` on a pty.
func startWizard(t *testing.T, extraEnv ...string) *ptySession {
	t.Helper()
	return startPTY(t, ptyOptions{Args: []string{"-c", "config theme"}, Env: extraEnv})
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
	s.stepUntil(selectLive, "\r", confirmLive) // take the focused option (plain)
	s.stepUntil(confirmLive, "\r", "saved to")

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
	s.stepUntil(selectLive, "\r", confirmLive)
	s.stepUntil(confirmLive, "\r", "nothing changed")
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
	s.stepUntil(selectLive, "\x1b[B\r", "separator preview") // down to p10k
	// Each step is driven until the *next* question appears, so a
	// keystroke lost to a raw-mode switch is simply resent.
	s.stepUntil(selectLive, "\r", "layout")             // keep plain separators
	s.stepUntil(selectLive, "\r", "frame")              // layout: keep 2
	s.stepUntil(selectLive, "\r", "segments, in order") // frame: keep on
	s.stepUntil("segments, in order", "\r", "preview:") // keep the default list
	s.stepUntil(confirmLive, "\r", "saved to")
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
	s.stepUntil(selectLive, "\x03", "nothing saved") // Ctrl-C

	out := s.plain()
	if bytes.Contains([]byte(out), []byte("saved to")) {
		t.Errorf("abort wrote the rc file:\n%s", out)
	}
}
