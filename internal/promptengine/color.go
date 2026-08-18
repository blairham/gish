package promptengine

import (
	"strconv"
	"strings"
)

// Color handling. Upstream accepts three spellings for every color
// setting and koi accepts the same three, because presets in the wild
// use all of them:
//
//	4          an xterm-256 index (0-255)
//	#1e66f5    a 24-bit hex triple
//	blue       a name, optionally prefixed bright- (or upstream's br)
//
// Indices render as 38;5;N rather than the terse 30-37 range even for
// 0-7: it is the same color on every terminal that supports either,
// and one code path is one fewer thing to get wrong.

// Color is a resolved color, or the unset zero value.
type Color struct {
	set    bool
	params string // SGR parameters without the leading ESC[ or trailing m
}

// Set reports whether a color was resolved at all. An unset Color emits
// nothing, which leaves the terminal's default in place.
func (c Color) Set() bool { return c.set }

// baseColors are the names every preset can rely on. Anything else is
// expected to be numeric or hex; an unknown name resolves to unset
// rather than to a wrong color.
var baseColors = map[string]int{
	"black": 0, "red": 1, "green": 2, "yellow": 3,
	"blue": 4, "magenta": 5, "cyan": 6, "white": 7,
}

// ParseColor resolves a color spec. ok is false for the empty string
// and for anything unrecognized, so callers can fall back rather than
// paint something the user did not ask for.
func ParseColor(spec string) (Color, bool) {
	s := strings.ToLower(strings.TrimSpace(spec))
	if s == "" {
		return Color{}, false
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 255 {
		return Color{set: true, params: "5;" + strconv.Itoa(n)}, true
	}
	if rgb, ok := parseHex(s); ok {
		return Color{set: true, params: "2;" + rgb}, true
	}
	// Names, with the bright variants upstream spells br/bright-.
	bright := false
	for _, prefix := range []string{"bg-", "fg-"} {
		s = strings.TrimPrefix(s, prefix)
	}
	switch {
	case strings.HasPrefix(s, "bright-"):
		s, bright = strings.TrimPrefix(s, "bright-"), true
	case strings.HasPrefix(s, "br") && len(s) > 2:
		if _, known := baseColors[strings.TrimPrefix(s, "br")]; known {
			s, bright = strings.TrimPrefix(s, "br"), true
		}
	}
	n, ok := baseColors[s]
	if !ok {
		return Color{}, false
	}
	if bright {
		n += 8
	}
	return Color{set: true, params: "5;" + strconv.Itoa(n)}, true
}

// parseHex accepts #rgb and #rrggbb, returning "r;g;b" decimal params.
func parseHex(s string) (string, bool) {
	digits, ok := strings.CutPrefix(s, "#")
	if !ok {
		return "", false
	}
	if len(digits) == 3 { // #abc is #aabbcc
		var expanded strings.Builder
		for _, r := range digits {
			expanded.WriteRune(r)
			expanded.WriteRune(r)
		}
		digits = expanded.String()
	}
	if len(digits) != 6 {
		return "", false
	}
	parts := make([]string, 3)
	for i := range parts {
		v, err := strconv.ParseUint(digits[i*2:i*2+2], 16, 8)
		if err != nil {
			return "", false
		}
		parts[i] = strconv.FormatUint(v, 10)
	}
	return strings.Join(parts, ";"), true
}

// Style is the full appearance of one piece of text.
type Style struct {
	Fg, Bg Color
	Bold   bool
}

// Empty reports whether the style would emit no escapes at all.
func (s Style) Empty() bool { return !s.Fg.set && !s.Bg.set && !s.Bold }

// sgr returns the escape sequence that enters the style, or "" when
// there is nothing to enter.
func (s Style) sgr() string {
	var params []string
	if s.Bold {
		params = append(params, "1")
	}
	if s.Fg.set {
		params = append(params, "38;"+s.Fg.params)
	}
	if s.Bg.set {
		params = append(params, "48;"+s.Bg.params)
	}
	if len(params) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(params, ";") + "m"
}

// Apply wraps text in the style. Empty text stays empty — a segment that
// declined to render must not leave stray escapes behind, which is what
// makes the "did anything render?" checks in the layout pass reliable.
func (s Style) Apply(text string) string {
	if text == "" || s.Empty() {
		return text
	}
	return s.sgr() + text + reset
}

const reset = "\x1b[0m"

// SegmentStyle resolves a segment's colors from the config through the
// standard fallback chain, with the caller's defaults underneath.
func (c *Config) SegmentStyle(segment, state string, defFg, defBg string) Style {
	var s Style
	s.Fg, _ = ParseColor(c.Param(segment, state, "FOREGROUND", defFg))
	s.Bg, _ = ParseColor(c.Param(segment, state, "BACKGROUND", defBg))
	s.Bold = c.ParamBool(segment, state, "BOLD", false)
	return s
}
