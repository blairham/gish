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

	p, cont := themedPrompt(info, defaultThemeConfig())
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
	p, _ := themedPrompt(themeInfo(), defaultThemeConfig())
	for _, banned := range []string{"✘", "⚙", "@"} {
		if strings.Contains(p, banned) {
			t.Errorf("clean prompt should not contain %q: %q", banned, p)
		}
	}
}

func TestThemeSegmentsPickAndOrder(t *testing.T) {
	info := themeInfo()
	info.segment = func(id string) string {
		if id == "git" {
			return "main !2"
		}
		return ""
	}
	info.exitCode = 7

	// exit before dir, git dropped entirely.
	p, _ := themedPrompt(info, themeConfig{segments: []string{"exit", "dir"}})
	exitAt, dirAt := strings.Index(p, "✘ 7"), strings.Index(p, "~/dev/gish")
	if exitAt == -1 || dirAt == -1 || exitAt > dirAt {
		t.Errorf("segment order not respected (exit@%d dir@%d):\n%q", exitAt, dirAt, p)
	}
	if strings.Contains(p, "main !2") {
		t.Errorf("dropped git segment still renders: %q", p)
	}
}

func TestThemePluginSegmentByID(t *testing.T) {
	info := themeInfo()
	info.segment = func(id string) string {
		if id == "k8s" {
			return "prod-cluster"
		}
		return ""
	}
	p, _ := themedPrompt(info, themeConfig{segments: []string{"dir", "k8s"}})
	if !strings.Contains(p, "prod-cluster") {
		t.Errorf("plugin segment id not rendered: %q", p)
	}
}

func TestThemeColorOverride(t *testing.T) {
	cfg := themeConfig{
		segments: []string{"dir"},
		colors:   map[string]string{"dir": "\x1b[33m"},
	}
	p, _ := themedPrompt(themeInfo(), cfg)
	if !strings.Contains(p, "\x1b[33m~/dev/gish") {
		t.Errorf("color override not applied: %q", p)
	}
	if strings.Contains(p, cCyan+"~/dev/gish") {
		t.Errorf("default color still applied over override: %q", p)
	}
}

func TestThemeConfigFrom(t *testing.T) {
	runner := newTestRunner(t)
	script := `GISH_THEME_SEGMENTS='git dir'
GISH_THEME_COLOR_DIR=yellow
GISH_THEME_COLOR_GIT='; rm -rf'`
	if err := runner.Run(t.Context(), parseLine(t, script)); err != nil {
		t.Fatal(err)
	}
	cfg := themeConfigFrom(runner)
	if got := strings.Join(cfg.segments, " "); got != "git dir" {
		t.Errorf("segments = %q, want %q", got, "git dir")
	}
	if got := cfg.colors["dir"]; got != "\x1b[33m" {
		t.Errorf("dir color = %q, want yellow SGR", got)
	}
	if _, ok := cfg.colors["git"]; ok {
		t.Error("invalid color value should be ignored, not applied")
	}

	// Unset variables mean the default config.
	def := themeConfigFrom(newTestRunner(t))
	if got := strings.Join(def.segments, " "); got != "dir git pins jobs duration exit" {
		t.Errorf("default segments = %q", got)
	}
}

func TestColorSGR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  string
		ok    bool
	}{
		{"cyan", "\x1b[36m", true},
		{"bright-red", "\x1b[91m", true},
		{"dim", "\x1b[2m", true},
		{"38;5;208", "\x1b[38;5;208m", true},
		{"", "", false},
		{"rainbow", "", false},
		{"31;evil", "", false},
	}
	for _, tt := range tests {
		got, ok := colorSGR(tt.value)
		if got != tt.want || ok != tt.ok {
			t.Errorf("colorSGR(%q) = %q, %v — want %q, %v", tt.value, got, ok, tt.want, tt.ok)
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
