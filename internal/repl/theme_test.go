package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func themeInfo() promptInfo {
	return promptInfo{
		username: "blair",
		host:     "mba",
		home:     "/home/blair",
		dir:      "/home/blair/dev/gish",
	}
}

func TestThemedPromptLayout(t *testing.T) {
	info := themeInfo()
	info.segment = func(id string) string {
		if id == "git" {
			return "main !2"
		}
		return ""
	}
	info.exitCode = 7
	info.duration = 5 * time.Second
	info.jobs = 2

	p, cont := themedPrompt(info)
	for _, want := range []string{"~/dev/gish", "main !2", "✘ 7", "5.0s", "⚙2", "\n", "❯"} {
		if !strings.Contains(p, want) {
			t.Errorf("themed prompt missing %q:\n%q", want, p)
		}
	}
	if strings.Count(p, "\n") != 1 {
		t.Errorf("themed prompt should be two lines: %q", p)
	}
	if !strings.Contains(cont, "│") {
		t.Errorf("cont prompt = %q", cont)
	}
}

func TestThemedPromptQuietWhenClean(t *testing.T) {
	p, _ := themedPrompt(themeInfo())
	for _, banned := range []string{"✘", "⚙", "@"} {
		if strings.Contains(p, banned) {
			t.Errorf("clean prompt should not contain %q: %q", banned, p)
		}
	}
}

func TestPromptStringsPrecedence(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	info := themeInfo()

	// Default: naked — the stock zsh/bash shape, no theme until asked.
	runner := newTestRunner(t)
	p, _ := promptStrings(runner, info)
	if p != "blair@mba gish % " {
		t.Errorf("default prompt not naked: %q", p)
	}

	// GISH_THEME=p10k: the native theme, opt-in.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=p10k`)); err != nil {
		t.Fatal(err)
	}
	if p, _ = promptStrings(runner, info); !strings.Contains(p, "❯") {
		t.Errorf("p10k theme not themed: %q", p)
	}

	// GISH_THEME=plain: same naked prompt, explicitly.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=plain`)); err != nil {
		t.Fatal(err)
	}
	if p, _ = promptStrings(runner, info); p != "blair@mba gish % " {
		t.Errorf("plain theme = %q, want naked", p)
	}

	// Manual GISH_PROMPT beats everything.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=p10k; GISH_PROMPT='mine> '`)); err != nil {
		t.Fatal(err)
	}
	if p, _ = promptStrings(runner, info); p != "mine> " {
		t.Errorf("manual prompt = %q, want %q", p, "mine> ")
	}
}

func TestPromptStringsRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	runner := newTestRunner(t)
	// Even with the theme opted in, NO_COLOR forces the naked prompt.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=p10k`)); err != nil {
		t.Fatal(err)
	}
	p, _ := promptStrings(runner, themeInfo())
	if p != "blair@mba gish % " {
		t.Errorf("NO_COLOR prompt = %q, want naked", p)
	}
}

func TestSmartPath(t *testing.T) {
	t.Parallel()

	tests := []struct{ dir, want string }{
		{"/home/blair", "~"},
		{"/home/blair/dev", "~/dev"},
		{"/home/blair/dev/gish", "~/dev/gish"},
		{"/home/blair/Developer/github.com/blairham/gish", "~/D/g/blairham/gish"},
		{"/etc/nginx/conf.d", "/etc/nginx/conf.d"},
	}
	for _, tt := range tests {
		if got := smartPath(tt.dir, "/home/blair"); got != tt.want {
			t.Errorf("smartPath(%q) = %q, want %q", tt.dir, got, tt.want)
		}
	}
}

func TestFmtDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		d    time.Duration
		want string
	}{
		{2300 * time.Millisecond, "2.3s"},
		{72 * time.Second, "1m12s"},
		{63 * time.Minute, "1h3m"},
	}
	for _, tt := range tests {
		if got := fmtDuration(tt.d); got != tt.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestToolPins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if got := toolPins(dir); got != "" {
		t.Errorf("no file = %q", got)
	}
	content := "# comment\ngolang 1.26.1\nnodejs 22.1.0\n"
	if err := os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "golang 1.26.1 nodejs 22.1.0"
	if got := toolPins(dir); got != want {
		t.Errorf("toolPins = %q, want %q", got, want)
	}
	// Cached second read.
	if got := toolPins(dir); got != want {
		t.Errorf("cached toolPins = %q", got)
	}
}
