package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateP10kConfig points the native config at a temp directory. Every
// test in this file needs it: without it the engine would read the
// developer's own ~/.config/koi/p10k.conf and the suite would pass or
// fail depending on whose machine it ran on.
func isolateP10kConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	return dir
}

func TestP10kThemeRenders(t *testing.T) {
	isolateP10kConfig(t)
	runner := newTestRunner(t)
	if err := runner.Run(t.Context(), parseLine(t, `KOI_THEME=p10k`)); err != nil {
		t.Fatal(err)
	}

	p, cp, _ := promptStrings(runner, themeInfo())
	if !strings.Contains(p, "❯") {
		t.Errorf("p10k prompt has no prompt character: %q", p)
	}
	// Two lines, and a blank one above: the shape people recognize.
	if strings.Count(p, "\n") != 2 {
		t.Errorf("p10k prompt should be a blank line plus two lines, got %q", p)
	}
	if cp == "" {
		t.Error("p10k theme produced no continuation prompt")
	}
}

func TestP10kPresetSelection(t *testing.T) {
	isolateP10kConfig(t)
	runner := newTestRunner(t)

	// robbyrussell is single-line and starts with its arrow: a shape
	// distinct enough that the preset is provably in effect.
	if err := runner.Run(t.Context(), parseLine(t, `KOI_THEME=p10k; KOI_P10K_PRESET=robbyrussell`)); err != nil {
		t.Fatal(err)
	}
	p, _, _ := promptStrings(runner, themeInfo())
	if strings.Contains(p, "\n") {
		t.Errorf("robbyrussell should be one line, got %q", p)
	}
	if !strings.Contains(p, "➜") {
		t.Errorf("robbyrussell arrow missing: %q", p)
	}
}

func TestP10kUnknownPresetDegrades(t *testing.T) {
	isolateP10kConfig(t)
	runner := newTestRunner(t)
	if err := runner.Run(t.Context(), parseLine(t, `KOI_THEME=p10k; KOI_P10K_PRESET=nonsense`)); err != nil {
		t.Fatal(err)
	}
	// A typo costs you the preset you asked for, never the prompt.
	if p, _, _ := promptStrings(runner, themeInfo()); !strings.Contains(p, "❯") {
		t.Errorf("unknown preset did not fall back to a working prompt: %q", p)
	}
}

func TestP10kSessionOverride(t *testing.T) {
	isolateP10kConfig(t)
	runner := newTestRunner(t)
	// The settings someone already knows how to type, read as data.
	script := `KOI_THEME=p10k; POWERLEVEL9K_LEFT_PROMPT_ELEMENTS='dir'; POWERLEVEL9K_PROMPT_ADD_NEWLINE=false`
	if err := runner.Run(t.Context(), parseLine(t, script)); err != nil {
		t.Fatal(err)
	}
	p, _, _ := promptStrings(runner, themeInfo())
	if strings.Contains(p, "❯") {
		t.Errorf("prompt_char was removed from the elements but still rendered: %q", p)
	}
	if strings.Contains(p, "\n") {
		t.Errorf("PROMPT_ADD_NEWLINE=false should leave no blank line: %q", p)
	}
}

func TestP10kNativeConfigFileApplies(t *testing.T) {
	dir := isolateP10kConfig(t)
	path := filepath.Join(dir, "koi", "p10k.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	conf := "# test\nLEFT_PROMPT_ELEMENTS = dir\nPROMPT_ADD_NEWLINE = false\nDIR_FOREGROUND = 201\n"
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := newTestRunner(t)
	if err := runner.Run(t.Context(), parseLine(t, `KOI_THEME=p10k`)); err != nil {
		t.Fatal(err)
	}
	p, _, _ := promptStrings(runner, themeInfo())
	if !strings.Contains(p, "38;5;201") {
		t.Errorf("config file color not applied: %q", p)
	}
	if strings.Contains(p, "❯") {
		t.Errorf("config file element list not applied: %q", p)
	}
}

func TestP10kSessionBeatsConfigFile(t *testing.T) {
	dir := isolateP10kConfig(t)
	path := filepath.Join(dir, "koi", "p10k.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("DIR_FOREGROUND = 201\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := newTestRunner(t)
	if err := runner.Run(t.Context(), parseLine(t, `KOI_THEME=p10k; POWERLEVEL9K_DIR_FOREGROUND=202`)); err != nil {
		t.Fatal(err)
	}
	p, _, _ := promptStrings(runner, themeInfo())
	if !strings.Contains(p, "38;5;202") || strings.Contains(p, "38;5;201") {
		t.Errorf("session setting should beat the config file: %q", p)
	}
}

func TestP10kNoColorStillNaked(t *testing.T) {
	isolateP10kConfig(t)
	t.Setenv("NO_COLOR", "1")
	runner := newTestRunner(t)
	if err := runner.Run(t.Context(), parseLine(t, `KOI_THEME=p10k`)); err != nil {
		t.Fatal(err)
	}
	// The degradation rule outranks the theme, exactly as before.
	if p, _, _ := promptStrings(runner, themeInfo()); p != "blair@mba koi % " {
		t.Errorf("NO_COLOR prompt = %q, want naked", p)
	}
}
