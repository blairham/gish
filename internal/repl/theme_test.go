package repl

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func themeInfo() promptInfo {
	return promptInfo{
		username: "blair",
		host:     "mba",
		home:     filepath.FromSlash("/home/blair"),
		dir:      filepath.FromSlash("/home/blair/dev/gish"),
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
	for _, want := range []string{filepath.FromSlash("~/dev/gish"), "main !2", "✘ 7", "5.0s", "⚙2", "\n", "❯"} {
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
	exitAt, dirAt := strings.Index(p, "✘ 7"), strings.Index(p, filepath.FromSlash("~/dev/gish"))
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
	if !strings.Contains(p, "\x1b[33m"+filepath.FromSlash("~/dev/gish")) {
		t.Errorf("color override not applied: %q", p)
	}
	if strings.Contains(p, cCyan+filepath.FromSlash("~/dev/gish")) {
		t.Errorf("default color still applied over override: %q", p)
	}
}

func TestThemeOneLineLayout(t *testing.T) {
	info := themeInfo()
	info.exitCode = 7
	p, _ := themedPrompt(info, themeConfig{segments: []string{"dir", "exit"}, oneLine: true})
	if strings.Contains(p, "\n") || strings.Contains(p, "╭") || strings.Contains(p, "╰") {
		t.Errorf("one-line layout still framed: %q", p)
	}
	if !strings.HasSuffix(p, "❯"+cReset+" ") {
		t.Errorf("one-line prompt should end with the arrow: %q", p)
	}
	if !strings.Contains(p, filepath.FromSlash("~/dev/gish")) || !strings.Contains(p, "✘ 7") {
		t.Errorf("one-line prompt missing segments: %q", p)
	}
}

func TestThemePowerlineSeparator(t *testing.T) {
	info := themeInfo()
	info.exitCode = 7
	cfg := themeConfig{segments: []string{"dir", "exit"}, powerline: true}
	p, _ := themedPrompt(info, cfg)
	if !strings.Contains(p, "\ue0b1") {
		t.Errorf("powerline separator missing: %q", p)
	}
	// A single rendered segment gets no separator.
	p, _ = themedPrompt(themeInfo(), themeConfig{segments: []string{"dir"}, powerline: true})
	if strings.Contains(p, "\ue0b1") {
		t.Errorf("separator rendered with one segment: %q", p)
	}
}

func TestThemeConfigFrom(t *testing.T) {
	runner := newTestRunner(t)
	script := `GISH_THEME_SEGMENTS='git dir'
GISH_THEME_COLOR_DIR=yellow
GISH_THEME_COLOR_GIT='; rm -rf'
GISH_THEME_LINES=1
GISH_THEME_SEP=powerline`
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
	if !cfg.oneLine || !cfg.powerline {
		t.Errorf("layout vars not read: oneLine=%v powerline=%v", cfg.oneLine, cfg.powerline)
	}

	// Unset variables mean the default config.
	def := themeConfigFrom(newTestRunner(t))
	if got := strings.Join(def.segments, " "); got != "dir git pins jobs duration exit" {
		t.Errorf("default segments = %q", got)
	}
	if def.oneLine || def.powerline {
		t.Errorf("defaults should be two-line plain: oneLine=%v powerline=%v", def.oneLine, def.powerline)
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
	p, _, _ := promptStrings(runner, info)
	if p != "blair@mba gish % " {
		t.Errorf("default prompt not naked: %q", p)
	}

	// GISH_THEME=p10k: the native theme, opt-in.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=p10k`)); err != nil {
		t.Fatal(err)
	}
	if p, _, _ = promptStrings(runner, info); !strings.Contains(p, "❯") {
		t.Errorf("p10k theme not themed: %q", p)
	}

	// GISH_THEME=plain: same naked prompt, explicitly.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=plain`)); err != nil {
		t.Fatal(err)
	}
	if p, _, _ = promptStrings(runner, info); p != "blair@mba gish % " {
		t.Errorf("plain theme = %q, want naked", p)
	}

	// Manual GISH_PROMPT beats everything.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=p10k; GISH_PROMPT='mine> '`)); err != nil {
		t.Fatal(err)
	}
	if p, _, _ = promptStrings(runner, info); p != "mine> " {
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
	p, _, _ := promptStrings(runner, themeInfo())
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
		if got := smartPath(filepath.FromSlash(tt.dir), filepath.FromSlash("/home/blair")); got != filepath.FromSlash(tt.want) {
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
	base := t.TempDir()
	// A fake install tree: golang installed, nodejs not — the segment
	// shows the truth (#77), marking the uninstalled pin.
	t.Setenv("ASDF_DATA_DIR", filepath.Join(base, "asdf"))
	t.Setenv("MISE_DATA_DIR", filepath.Join(base, "mise"))
	if err := os.MkdirAll(filepath.Join(base, "asdf", "installs", "golang", "1.26.1", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(base, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := toolPins(dir); got != "" {
		t.Errorf("no file = %q", got)
	}
	content := "# comment\ngolang 1.26.1\nnodejs 22.1.0\n"
	if err := os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "golang 1.26.1 nodejs 22.1.0✗"
	if got := toolPins(dir); got != want {
		t.Errorf("toolPins = %q, want %q", got, want)
	}
	// Cached second read.
	if got := toolPins(dir); got != want {
		t.Errorf("cached toolPins = %q", got)
	}
}

func TestThemedRPrompt(t *testing.T) {
	info := themeInfo()
	info.exitCode = 3
	cfg := themeConfig{
		rprompt: []string{"exit", "time"},
		colors:  map[string]string{"exit": "\x1b[33m"},
	}
	rp := themedRPrompt(info, cfg)
	if !strings.Contains(rp, "\x1b[33m✘ 3") {
		t.Errorf("rprompt missing colored exit segment: %q", rp)
	}
	// The time segment renders HH:MM:SS.
	if !regexp.MustCompile(`\d{2}:\d{2}:\d{2}`).MatchString(rp) {
		t.Errorf("rprompt missing time segment: %q", rp)
	}
	// Empty rprompt config renders nothing.
	if got := themedRPrompt(info, themeConfig{}); got != "" {
		t.Errorf("empty rprompt config rendered %q", got)
	}
}

func TestRPromptString(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	info := themeInfo()

	// Plain theme: no right prompt.
	runner := newTestRunner(t)
	if _, _, rp := promptStrings(runner, info); rp != "" {
		t.Errorf("plain theme rprompt = %q, want empty", rp)
	}

	// p10k with configured segments.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=p10k; GISH_THEME_RPROMPT=time`)); err != nil {
		t.Fatal(err)
	}
	if _, _, rp := promptStrings(runner, info); rp == "" {
		t.Error("p10k rprompt empty despite GISH_THEME_RPROMPT=time")
	}

	// Manual GISH_PROMPT wins: the theme (and its rprompt) stand down.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_PROMPT='mine> '`)); err != nil {
		t.Fatal(err)
	}
	if _, _, rp := promptStrings(runner, info); rp != "" {
		t.Errorf("manual prompt rprompt = %q, want empty", rp)
	}
}

func TestThemeFramelessTwoLine(t *testing.T) {
	p, _ := themedPrompt(themeInfo(), themeConfig{segments: []string{"dir"}, noFrame: true})
	if strings.Contains(p, "╭") || strings.Contains(p, "╰") {
		t.Errorf("frameless layout still has corners: %q", p)
	}
	if strings.Count(p, "\n") != 1 {
		t.Errorf("frameless layout should stay two lines: %q", p)
	}
	if !strings.Contains(p, "❯") {
		t.Errorf("frameless layout missing arrow: %q", p)
	}
}

func TestPromptEscapeSet(t *testing.T) {
	t.Parallel()

	info := promptInfo{
		username: "blair",
		host:     "mba",
		home:     filepath.FromSlash("/home/blair"),
		dir:      filepath.FromSlash("/home/blair/dev/gish"),
		exitCode: 7,
		segment:  func(id string) string { return "seg-" + id },
	}
	tests := []struct{ format, want string }{
		// zsh spellings and gish spellings are the same escape.
		{"%n|%u", "blair|blair"},
		{"%m|%h", "mba|mba"},
		{"%~|%w", filepath.FromSlash("~/dev/gish") + "|" + filepath.FromSlash("~/dev/gish")},
		{"%d", filepath.FromSlash("/home/blair/dev/gish")},
		{"%W", "gish"},
		{"%?", "7"},
		{"%p{git}", "seg-git"},
		{"100%%", "100%"},
		// Unknown escapes pass through rather than vanishing.
		{"%x", "%x"},
		{"trailing%", "trailing%"},
	}
	for _, tt := range tests {
		if got := expandPrompt(tt.format, info); got != tt.want {
			t.Errorf("expandPrompt(%q) = %q, want %q", tt.format, got, tt.want)
		}
	}
	// %# is a user's % (this test never runs as root in CI, but assert
	// the shape rather than the euid).
	if got := expandPrompt("%#", info); got != "%" && got != "#" {
		t.Errorf("%%# = %q", got)
	}
}

func TestThemeNamePrecedence(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	runner := newTestRunner(t)

	if got := themeName(runner); got != "plain" {
		t.Errorf("default theme = %q", got)
	}
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=p10k`)); err != nil {
		t.Fatal(err)
	}
	if got := themeName(runner); got != "p10k" {
		t.Errorf("GISH_THEME ignored: %q", got)
	}
	// A manual prompt outranks the theme — one pipeline, one precedence.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_PROMPT='mine> '`)); err != nil {
		t.Fatal(err)
	}
	if got := themeName(runner); got != "literal" {
		t.Errorf("GISH_PROMPT did not win: %q", got)
	}
	// The literal theme renders through the same dispatch as any theme.
	p, cp, rp := promptStrings(runner, themeInfo())
	if p != "mine> " || rp != "" {
		t.Errorf("literal theme = %q / %q / %q", p, cp, rp)
	}
}

func TestColorlessTerminalForcesPlain(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	runner := newTestRunner(t)
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=p10k`)); err != nil {
		t.Fatal(err)
	}
	if got := themeName(runner); got != "plain" {
		t.Errorf("NO_COLOR did not force plain: %q", got)
	}
}
