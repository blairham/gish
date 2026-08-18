package promptengine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func isolateConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "koi", ConfigFileName)
}

// TestConfigRoundTripPreservesWhitespace is the regression this file
// exists for.
//
// Every lean-derived configuration separates segments by setting
// LEFT_SUBSEGMENT_SEPARATOR to a single space. Trimming values on read
// turned that space into "", and the prompt rendered with its segments
// run together — `~/dev/koimain` rather than `~/dev/koi main`. Both
// halves looked correct in isolation; only the round trip showed it.
func TestConfigRoundTripPreservesWhitespace(t *testing.T) {
	isolateConfig(t)

	want := map[string]string{
		"LEFT_SUBSEGMENT_SEPARATOR":     " ",
		"RIGHT_SUBSEGMENT_SEPARATOR":    " ",
		"LEFT_LEFT_WHITESPACE":          "",
		"DIR_FOREGROUND":                "31",
		"MULTILINE_LAST_PROMPT_PREFIX":  "%242F╰─",
		"COMMAND_EXECUTION_TIME_PREFIX": "took ",
	}
	cfg := NewConfig()
	for k, v := range want {
		cfg.Set(k, v)
	}

	if _, err := SaveNativeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got := LoadNativeConfig()
	if got == nil {
		t.Fatal("saved config did not load back")
	}
	for k, v := range want {
		if have := got.Str(k, "<missing>"); have != v {
			t.Errorf("%s round-tripped as %q, want %q", k, have, v)
		}
	}
}

// TestConfigRoundTripRendersTheSame is the same guarantee stated the way
// a user would notice it: save, reload, and the prompt is identical.
func TestConfigRoundTripRendersTheSame(t *testing.T) {
	isolateConfig(t)

	before := Preset("lean")
	if _, err := SaveNativeConfig(before); err != nil {
		t.Fatal(err)
	}
	after := LoadNativeConfig()
	if after == nil {
		t.Fatal("config did not load back")
	}

	ctx := sampleContext()
	if want, have := Render(before, ctx).Prompt, Render(after, sampleContext()).Prompt; want != have {
		t.Errorf("a saved-and-reloaded preset renders differently:\n before: %q\n  after: %q",
			plain(want), plain(have))
	}
}

func TestConfigRoundTripPreservesLists(t *testing.T) {
	isolateConfig(t)

	cfg := NewConfig()
	elements := []string{"dir", "vcs", elementNewline, "prompt_char"}
	cfg.SetList("LEFT_PROMPT_ELEMENTS", elements)
	if _, err := SaveNativeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got := LoadNativeConfig()
	if !slices.Equal(got.List("LEFT_PROMPT_ELEMENTS"), elements) {
		t.Errorf("elements round-tripped as %v", got.List("LEFT_PROMPT_ELEMENTS"))
	}
}

func TestConfigReadsHandWrittenValues(t *testing.T) {
	// The file is meant to be edited by hand, so both spellings have to
	// work: bare for the ordinary case, quoted when whitespace matters.
	dir := t.TempDir()
	path := filepath.Join(dir, "p10k.conf")
	body := strings.Join([]string{
		"# a comment",
		"",
		"DIR_FOREGROUND = 31",
		`LEFT_SUBSEGMENT_SEPARATOR = " "`,
		"LEFT_SEGMENT_SEPARATOR =",
		"TIME_FORMAT = %D{%H:%M:%S}",
		"not a setting at all",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfigFile(path)
	tests := []struct{ key, want string }{
		{"DIR_FOREGROUND", "31"},
		{"LEFT_SUBSEGMENT_SEPARATOR", " "},
		{"LEFT_SEGMENT_SEPARATOR", ""},
		{"TIME_FORMAT", "%D{%H:%M:%S}"},
	}
	for _, tt := range tests {
		if got := cfg.Str(tt.key, "<missing>"); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.key, got, tt.want)
		}
	}
	// A line that is not a setting is reported, not silently skipped.
	if len(cfg.Unsupported) != 1 {
		t.Errorf("expected one unreadable line, got %v", cfg.Unsupported)
	}
}

// TestImportedConfigRoundTripsIntoTheSamePrompt is the whole user
// journey: a .p10k.zsh in, a native config written, and the prompt that
// comes back out is the one the import produced.
func TestImportedConfigRoundTripsIntoTheSamePrompt(t *testing.T) {
	isolateConfig(t)

	imported := Preset("lean")
	imported.Merge(importFixture(t))
	direct := Render(imported, sampleContext()).Prompt

	if _, err := SaveNativeConfig(imported); err != nil {
		t.Fatal(err)
	}
	reloaded := LoadNativeConfig()
	if reloaded == nil {
		t.Fatal("imported config did not load back")
	}
	if got := Render(reloaded, sampleContext()).Prompt; got != direct {
		t.Errorf("imported config renders differently after a save:\n direct: %q\n saved:  %q",
			plain(direct), plain(got))
	}
}
