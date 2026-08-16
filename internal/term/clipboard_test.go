package term

import (
	"encoding/base64"
	"strings"
	"testing"
)

// clipWriter stands in for a terminal so the emitted bytes can be read.
type clipWriter struct{ strings.Builder }

func TestSetClipboardEmitsOSC52(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("STY", "")
	t.Setenv("TERM", "xterm-256color")

	var w clipWriter
	if err := SetClipboard(&w, "hello world"); err != nil {
		t.Fatal(err)
	}
	got := w.String()
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello world")) + "\x07"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSetPrimaryUsesPrimarySelection(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("STY", "")
	var w clipWriter
	if err := SetPrimary(&w, "x"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.String(), "\x1b]52;p;") {
		t.Errorf("primary selection not used: %q", w.String())
	}
}

// Inside tmux the sequence must be re-framed or tmux eats it, and every
// inner ESC has to be doubled.
func TestTmuxPassthroughWrapping(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	t.Setenv("STY", "")

	var w clipWriter
	if err := SetClipboard(&w, "hi"); err != nil {
		t.Fatal(err)
	}
	got := w.String()
	if !strings.HasPrefix(got, "\x1bPtmux;") {
		t.Errorf("missing tmux DCS wrapper: %q", got)
	}
	if !strings.HasSuffix(got, "\x1b\\") {
		t.Errorf("missing string terminator: %q", got)
	}
	if !strings.Contains(got, "\x1b\x1b]52;c;") {
		t.Errorf("inner ESC was not doubled: %q", got)
	}
}

func TestScreenPassthroughWrapping(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("STY", "1234.pts-0.host")
	t.Setenv("TERM", "screen.xterm-256color")

	var w clipWriter
	if err := SetClipboard(&w, "hi"); err != nil {
		t.Fatal(err)
	}
	if got := w.String(); !strings.HasPrefix(got, "\x1bP\x1b]52;") {
		t.Errorf("screen wrapper wrong: %q", got)
	}
}

// A payload past the cap is refused rather than truncated: a truncated
// base64 blob decodes to garbage, which is worse than an error.
func TestOversizedPayloadIsRefusedNotTruncated(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("STY", "")

	var w clipWriter
	err := SetClipboard(&w, strings.Repeat("x", maxClipboardBytes))
	if err == nil {
		t.Fatal("oversized payload was accepted")
	}
	if w.Len() != 0 {
		t.Errorf("bytes were emitted despite the error: %q", w.String())
	}
	if !strings.Contains(err.Error(), "over") {
		t.Errorf("error does not explain the cap: %v", err)
	}
}

// The degradation gate, matching every other escape-emitting surface.
func TestClipboardWritableGate(t *testing.T) {
	var w clipWriter
	if ClipboardWritable(&w) {
		t.Error("a non-file writer was accepted")
	}
	t.Setenv("NO_COLOR", "1")
	if ClipboardWritable(&w) {
		t.Error("NO_COLOR was ignored")
	}
}

// There is deliberately no clipboard *read*. This test is the record of
// that decision: a shell that can read the clipboard can exfiltrate
// whatever was last copied, which is why terminals gate OSC 52 at all.
func TestNoClipboardReadExists(t *testing.T) {
	// If someone adds a query helper, this file is where they should
	// have to argue for it. The write path never emits the read form.
	t.Setenv("TMUX", "")
	t.Setenv("STY", "")
	var w clipWriter
	if err := SetClipboard(&w, "x"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.String(), "?") {
		t.Errorf("the query form (OSC 52 ... ?) was emitted: %q", w.String())
	}
}
