package p10k

import (
	"strings"
	"testing"
)

func TestParamFallbackChain(t *testing.T) {
	c := NewConfig()
	c.Set("FOREGROUND", "global")
	c.Set("DIR_FOREGROUND", "segment")
	c.Set("DIR_NOT_WRITABLE_FOREGROUND", "state")

	tests := []struct {
		segment, state, want string
	}{
		{"dir", "NOT_WRITABLE", "state"},
		{"dir", "", "segment"},
		{"dir", "OTHER", "segment"}, // unknown state falls back to the segment
		{"vcs", "", "global"},       // unconfigured segment falls back to global
		{"", "", "global"},          // no segment at all is the global lookup
	}
	for _, tt := range tests {
		if got := c.Param(tt.segment, tt.state, "FOREGROUND", "default"); got != tt.want {
			t.Errorf("Param(%q,%q) = %q, want %q", tt.segment, tt.state, got, tt.want)
		}
	}
	if got := c.Param("dir", "", "BACKGROUND", "fallback"); got != "fallback" {
		t.Errorf("unset key should reach the caller's default, got %q", got)
	}
}

func TestParamChainNormalizesSpelling(t *testing.T) {
	c := NewConfig()
	// However a setting is spelled on the way in, it is one setting.
	c.Set("POWERLEVEL9K_DIR_FOREGROUND", "a")
	if got := c.Param("dir", "", "FOREGROUND", ""); got != "a" {
		t.Errorf("prefixed key not normalized: %q", got)
	}
	c.Set("dir_foreground", "b")
	if got := c.Param("DIR", "", "foreground", ""); got != "b" {
		t.Errorf("case not normalized: %q", got)
	}
	if keys := c.Keys(); len(keys) != 1 {
		t.Errorf("expected one canonical key, got %v", keys)
	}
}

func TestExplicitlyEmptyIsNotUnset(t *testing.T) {
	// Presets rely on this: setting a separator to "" means "no
	// separator", which must beat the renderer's default of " ".
	c := NewConfig()
	c.Set("LEFT_LEFT_WHITESPACE", "")
	if !c.Has("LEFT_LEFT_WHITESPACE") {
		t.Error("an empty value should still read as set")
	}
	if got := symbolOr(c, "dir", "LEFT_LEFT_WHITESPACE", " "); got != "" {
		t.Errorf("explicitly empty symbol became %q", got)
	}
	if got := symbolOr(NewConfig(), "dir", "LEFT_LEFT_WHITESPACE", " "); got != " " {
		t.Errorf("unset symbol should take the default, got %q", got)
	}
}

func TestBoolAndIntDegradeRatherThanBreak(t *testing.T) {
	c := NewConfig()
	c.Set("A", "yes")
	c.Set("B", "nonsense")
	c.Set("N", "twelve")
	if !c.Bool("A", false) {
		t.Error("yes should read as true")
	}
	if !c.Bool("B", true) {
		t.Error("an unparsable bool should fall back to the default")
	}
	if got := c.Int("N", 7); got != 7 {
		t.Errorf("an unparsable int should fall back, got %d", got)
	}
}

func TestMergeLayersLaterOverEarlier(t *testing.T) {
	base := Preset("lean")
	over := NewConfig()
	over.Set("DIR_FOREGROUND", "99")
	over.SetList("LEFT_PROMPT_ELEMENTS", []string{"dir"})

	base.Merge(over)
	if got := base.Param("dir", "", "FOREGROUND", ""); got != "99" {
		t.Errorf("override lost: %q", got)
	}
	if got := base.List("LEFT_PROMPT_ELEMENTS"); len(got) != 1 || got[0] != "dir" {
		t.Errorf("a later list should replace, not append: %v", got)
	}
	if got := base.Param("vcs", "CLEAN", "FOREGROUND", ""); got != "76" {
		t.Errorf("untouched setting should survive the merge, got %q", got)
	}
}

func TestCloneIsDeep(t *testing.T) {
	base := Preset("lean")
	clone := base.Clone()
	clone.Set("DIR_FOREGROUND", "1")
	clone.SetList("LEFT_PROMPT_ELEMENTS", []string{"vcs"})
	if got := base.Param("dir", "", "FOREGROUND", ""); got != "31" {
		t.Errorf("clone wrote through to the original: %q", got)
	}
	if got := base.List("LEFT_PROMPT_ELEMENTS"); got[0] != "dir" {
		t.Errorf("clone shared its list with the original: %v", got)
	}
}

func TestParseColor(t *testing.T) {
	tests := []struct {
		spec, want string
		ok         bool
	}{
		{"4", "5;4", true},
		{"255", "5;255", true},
		{"256", "", false}, // out of range is not a color
		{"red", "5;1", true},
		{"bright-red", "5;9", true},
		{"brred", "5;9", true},
		{"#ff0000", "2;255;0;0", true},
		{"#f00", "2;255;0;0", true},
		{"", "", false},
		{"chartreuse", "", false},
	}
	for _, tt := range tests {
		got, ok := ParseColor(tt.spec)
		if ok != tt.ok || got.params != tt.want {
			t.Errorf("ParseColor(%q) = (%q,%v), want (%q,%v)", tt.spec, got.params, ok, tt.want, tt.ok)
		}
	}
}

func TestExpandMarkup(t *testing.T) {
	// The frame prefixes are the real-world case: a color, then text.
	got := Expand("%242F╭─", nil)
	if !strings.Contains(got, "38;5;242") || !strings.Contains(got, "╭─") {
		t.Errorf("frame prefix did not expand: %q", got)
	}
	if !strings.HasSuffix(got, reset) {
		t.Errorf("expansion should restore the terminal: %q", got)
	}

	if got := Expand("100%% sure", nil); got != "100% sure" {
		t.Errorf("%%%% should be a literal percent, got %q", got)
	}
	if got := Expand("%F{red}x%f", nil); !strings.Contains(got, "38;5;1") {
		t.Errorf("braced color did not expand: %q", got)
	}
	// An escape this expander does not implement must survive untouched
	// rather than be swallowed.
	if got := Expand("%D{%H}", nil); !strings.Contains(got, "%D") {
		t.Errorf("unknown escape was swallowed: %q", got)
	}
}

func TestExpandSubstitutesVariables(t *testing.T) {
	lookup := func(name string) string {
		if name == "P9K_CONTENT" {
			return "main"
		}
		return ""
	}
	if got := Expand("[${P9K_CONTENT}]", lookup); got != "[main]" {
		t.Errorf("substitution failed: %q", got)
	}
	// Anything that is not a plain name is a construct this expander
	// deliberately does not implement; it must not be half-evaluated.
	if got := Expand("${$((my_git_formatter(1)))+${x}}", lookup); !strings.Contains(got, "my_git_formatter") {
		t.Errorf("a shell construct should pass through visibly, got %q", got)
	}
}

func TestDecodeEscapes(t *testing.T) {
	if got := decodeEscapes(``); got != "" {
		t.Errorf("powerline glyph did not decode: %q", got)
	}
	if got := decodeEscapes(`a\qb`); got != `a\qb` {
		t.Errorf("unknown escape should be left alone, got %q", got)
	}
	if got := decodeEscapes("plain"); got != "plain" {
		t.Errorf("unescaped text changed: %q", got)
	}
}

func TestDisplayWidthIgnoresEscapes(t *testing.T) {
	styled := Style{Fg: mustColor("1")}.Apply("abc")
	if got := displayWidth(styled); got != 3 {
		t.Errorf("styled width = %d, want 3", got)
	}
	if got := displayWidth("\x1b]8;;http://example.com\x07link\x1b]8;;\x07"); got != 4 {
		t.Errorf("hyperlink width = %d, want 4", got)
	}
	if got := displayWidth("日本"); got != 4 {
		t.Errorf("wide characters = %d, want 4", got)
	}
}
