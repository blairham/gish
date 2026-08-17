package p10k

import (
	"strings"
	"testing"
)

func TestTransientOffByDefault(t *testing.T) {
	if _, ok := RenderTransient(Preset("lean"), sampleContext()); ok {
		t.Error("a preset should not trim prompts unless asked to")
	}
}

func TestTransientAlways(t *testing.T) {
	cfg := Preset("lean")
	cfg.Set("TRANSIENT_PROMPT", "always")

	got, ok := RenderTransient(cfg, sampleContext())
	if !ok {
		t.Fatal("TRANSIENT_PROMPT=always produced nothing")
	}
	// What is left is the prompt character and nothing else: no
	// directory, no branch, no clock.
	text := strings.TrimSpace(plain(got))
	if text != "❯" {
		t.Errorf("transient prompt = %q, want just the prompt character", text)
	}
	// The accepted line keeps the gap, so scrollback reads the same as
	// the live prompt did rather than "❯command".
	if !strings.HasSuffix(plain(got), "❯ ") {
		t.Errorf("transient prompt = %q, want a trailing space", plain(got))
	}
	for _, gone := range []string{"gish", "main", "14:05"} {
		if strings.Contains(plain(got), gone) {
			t.Errorf("transient prompt still carries %q: %q", gone, plain(got))
		}
	}
}

func TestTransientKeepsPromptCharColor(t *testing.T) {
	cfg := Preset("lean")
	cfg.Set("TRANSIENT_PROMPT", "always")
	ctx := sampleContext()
	ctx.ExitCode = 1

	got, ok := RenderTransient(cfg, ctx)
	if !ok {
		t.Fatal("no transient prompt")
	}
	// The failure color is the one thing worth keeping in scrollback.
	if !strings.Contains(got, "38;5;196") {
		t.Errorf("transient prompt lost the error color: %q", got)
	}
}

func TestTransientSameDir(t *testing.T) {
	cfg := Preset("lean")
	cfg.Set("TRANSIENT_PROMPT", "same-dir")

	ctx := sampleContext()
	ctx.PrevCwd = ctx.Cwd
	if _, ok := RenderTransient(cfg, ctx); !ok {
		t.Error("same-dir should trim when the directory did not change")
	}

	moved := sampleContext()
	moved.PrevCwd = "/fixture/you/elsewhere"
	if _, ok := RenderTransient(cfg, moved); ok {
		t.Error("same-dir should leave a full prompt as a landmark after a directory change")
	}
}

func TestTransientNeedsSomethingToShow(t *testing.T) {
	// An element list ending in a newline has an empty last line. Trimming
	// to nothing would leave the command sitting against the margin with
	// no marker, which is worse than not trimming at all.
	cfg := Preset("lean")
	cfg.Set("TRANSIENT_PROMPT", "always")
	cfg.SetList("LEFT_PROMPT_ELEMENTS", []string{"dir", elementNewline})
	if _, ok := RenderTransient(cfg, sampleContext()); ok {
		t.Error("an empty last line should not become the transient prompt")
	}
}

func TestTransientSingleLinePreset(t *testing.T) {
	// robbyrussell is one line, so its transient form is that whole line.
	cfg := Preset("robbyrussell")
	cfg.Set("TRANSIENT_PROMPT", "always")
	got, ok := RenderTransient(cfg, sampleContext())
	if !ok {
		t.Fatal("no transient prompt for a single-line preset")
	}
	if !strings.Contains(plain(got), "➜") {
		t.Errorf("transient prompt = %q", plain(got))
	}
}
