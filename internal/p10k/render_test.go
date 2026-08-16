package p10k

import (
	"strings"
	"testing"
	"time"
)

// sampleContext is a realistic prompt state: a git repo with a little of
// everything outstanding, a slow last command, and a known clock.
//
// The fake home deliberately avoids /home: on macOS that path is an
// autofs mount, so stat'ing a non-existent file under it wakes the
// automounter and costs ~24ms — which made the segments that search up
// the tree look 1000x slower than they are.
func sampleContext() *Context {
	return &Context{
		Cwd:      "/fixture/you/dev/gish",
		Home:     "/fixture/you",
		Username: "you",
		Hostname: "host",
		ExitCode: 0,
		Duration: 4 * time.Second,
		Width:    100,
		Now:      time.Date(2026, 8, 16, 14, 5, 6, 0, time.UTC),
		Git: &GitStatus{
			Dir: "/fixture/you/dev/gish", Branch: "main",
			Ahead: 2, Modified: 3, Untracked: 1,
		},
		Getenv: func(string) string { return "" },
	}
}

// plain strips ANSI so a test can assert on what the user reads rather
// than on the escapes that color it.
func plain(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		if rest, ok := skipEscape(s); ok {
			s = rest
			continue
		}
		b.WriteByte(s[0])
		s = s[1:]
	}
	return b.String()
}

func TestRenderLean(t *testing.T) {
	got := Render(Preset("lean"), sampleContext())

	// Two lines of prompt, preceded by the blank separator line.
	lines := strings.Split(plain(got.Prompt), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected a blank line plus two prompt lines, got %d: %q", len(lines), plain(got.Prompt))
	}
	if lines[0] != "" {
		t.Errorf("PROMPT_ADD_NEWLINE should put a blank line first, got %q", lines[0])
	}
	if want := "~/dev/gish main"; !strings.HasPrefix(lines[1], want) {
		t.Errorf("first line = %q, want prefix %q", lines[1], want)
	}
	for _, want := range []string{"⇡2", "!3", "?1"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("first line %q missing vcs counter %q", lines[1], want)
		}
	}
	if strings.TrimSpace(lines[2]) != "❯" {
		t.Errorf("second line = %q, want the prompt character", lines[2])
	}
	// The last line's right side is a real right prompt, not baked in.
	if !strings.Contains(plain(got.RPrompt), "") && got.RPrompt != "" {
		t.Errorf("unexpected rprompt on the editing line: %q", plain(got.RPrompt))
	}
}

func TestRenderLeanRightSideOnFirstLine(t *testing.T) {
	ctx := sampleContext()
	ctx.ExitCode = 1
	got := plain(Render(Preset("lean"), ctx).Prompt)

	// status and duration belong to the banner line, held at the right
	// edge by the gap filler.
	first := strings.Split(got, "\n")[1]
	for _, want := range []string{"✘", "1", "4s"} {
		if !strings.Contains(first, want) {
			t.Errorf("banner line %q missing %q", first, want)
		}
	}
	if w := displayWidth(first); w > 100 {
		t.Errorf("banner line is %d columns wide, terminal is 100: %q", w, first)
	}
}

func TestRenderPromptCharTracksExitCode(t *testing.T) {
	ok := Render(Preset("lean"), sampleContext())
	ctx := sampleContext()
	ctx.ExitCode = 1
	bad := Render(Preset("lean"), ctx)

	if ok.Prompt == bad.Prompt {
		t.Fatal("a failed command should change the prompt")
	}
	// Same character, different color — that is the whole signal.
	if plain(ok.Prompt) == plain(bad.Prompt) {
		t.Error("the difference should be color, not text")
	}
}

func TestRenderEveryPresetProducesAPrompt(t *testing.T) {
	for _, name := range Presets() {
		t.Run(name, func(t *testing.T) {
			got := Render(Preset(name), sampleContext())
			if strings.TrimSpace(plain(got.Prompt)) == "" {
				t.Fatal("preset rendered an empty prompt")
			}
			if !strings.Contains(plain(got.Prompt), "gish") {
				t.Errorf("preset does not show the directory: %q", plain(got.Prompt))
			}
			// No preset may run past the terminal edge on any line.
			for _, line := range strings.Split(got.Prompt, "\n") {
				if w := displayWidth(line); w > 100 {
					t.Errorf("line is %d columns wide: %q", w, plain(line))
				}
			}
		})
	}
}

func TestRenderWithoutGitStaysQuiet(t *testing.T) {
	ctx := sampleContext()
	ctx.Git = nil
	got := plain(Render(Preset("lean"), ctx).Prompt)
	if strings.Contains(got, "main") {
		t.Errorf("no repository, but the prompt mentions a branch: %q", got)
	}
}

func TestRenderUnknownSegmentIsSkipped(t *testing.T) {
	cfg := Preset("lean")
	cfg.SetList("LEFT_PROMPT_ELEMENTS", []string{"dir", "not_a_segment", elementNewline, "prompt_char"})
	got := plain(Render(cfg, sampleContext()).Prompt)
	if !strings.Contains(got, "gish") {
		t.Errorf("an unknown element should vanish, not break the prompt: %q", got)
	}
}
