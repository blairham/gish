package promptengine

import (
	"strconv"
	"strings"
)

// Config values are markup, and this file is the reader for it.
//
// Presets do not store "the frame prefix is ╭─ in color 242" as two
// settings; they store the string '%242F╭─'. That %-markup is zsh's
// prompt-escape format, and honoring it is not the same thing as
// interpreting zsh: it is a color markup with a fixed vocabulary, in
// the same family as koi's own %-escapes in internal/repl/theme.go.
// The subset below is what the presets actually use — anything outside
// it passes through untouched rather than being guessed at.
//
//	%F{c} %<n>F   foreground on        %f   foreground off
//	%K{c} %<n>K   background on        %k   background off
//	%B %b         bold on / off        %U %u   underline on / off
//	%%            a literal %
//
// Substitution is deliberately limited to ${NAME} against a caller-
// supplied lookup, which is how CONTENT_EXPANSION reaches P9K_CONTENT
// and P9K_VISUAL_IDENTIFIER. There is no command substitution, no
// arithmetic, and no function call — those are the parts of a .p10k.zsh
// that cannot come along, and they are reported at import time.

// expandState is the running appearance while walking a markup string.
type expandState struct {
	fg, bg    Color
	bold      bool
	underline bool
}

// sgr renders the current appearance, or a plain reset when nothing is
// active. Emitting the full state at every change (rather than deltas)
// keeps the output correct when pieces are concatenated later.
func (s expandState) sgr() string {
	var params []string
	if s.bold {
		params = append(params, "1")
	}
	if s.underline {
		params = append(params, "4")
	}
	if s.fg.set {
		params = append(params, "38;"+s.fg.params)
	}
	if s.bg.set {
		params = append(params, "48;"+s.bg.params)
	}
	if len(params) == 0 {
		return reset
	}
	return "\x1b[0;" + strings.Join(params, ";") + "m"
}

// Expand turns a config markup string into ANSI text. lookup resolves
// ${NAME} references and may be nil, in which case references expand to
// nothing — an unset variable is empty, never the literal text.
func Expand(s string, lookup func(string) string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	var st expandState
	touched := false

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == '$' && i+1 < len(runes):
			name, next, ok := varRef(runes, i)
			if !ok {
				b.WriteRune(runes[i])
				continue
			}
			i = next
			if lookup != nil {
				b.WriteString(lookup(name))
			}
		case runes[i] == '%' && i+1 < len(runes):
			consumed, changed := expandEscape(runes, i, &st, &b)
			if consumed == i { // not an escape we know: pass it through
				b.WriteRune(runes[i])
				continue
			}
			i = consumed
			if changed {
				touched = true
				b.WriteString(st.sgr())
			}
		default:
			b.WriteRune(runes[i])
		}
	}
	out := b.String()
	// Leave the terminal as we found it, but only if we changed it.
	if touched && !strings.HasSuffix(out, reset) {
		out += reset
	}
	return out
}

// expandEscape handles one %-escape starting at i. It returns the index
// of the escape's last rune (or i when this is not a recognized escape)
// and whether the appearance changed.
func expandEscape(runes []rune, i int, st *expandState, b *strings.Builder) (int, bool) {
	// Numeric prefix form: %242F and %3K.
	j := i + 1
	digits := j
	for digits < len(runes) && runes[digits] >= '0' && runes[digits] <= '9' {
		digits++
	}
	if digits > j && digits < len(runes) {
		spec := string(runes[j:digits])
		switch runes[digits] {
		case 'F':
			st.fg, _ = ParseColor(spec)
			return digits, true
		case 'K':
			st.bg, _ = ParseColor(spec)
			return digits, true
		}
	}

	switch runes[j] {
	case '%':
		b.WriteByte('%')
		return j, false
	case 'f':
		st.fg = Color{}
		return j, true
	case 'k':
		st.bg = Color{}
		return j, true
	case 'B':
		st.bold = true
		return j, true
	case 'b':
		st.bold = false
		return j, true
	case 'U':
		st.underline = true
		return j, true
	case 'u':
		st.underline = false
		return j, true
	case 'F', 'K':
		arg, next, ok := bracedArg(runes, j)
		if !ok {
			return i, false
		}
		col, _ := ParseColor(arg)
		if runes[j] == 'F' {
			st.fg = col
		} else {
			st.bg = col
		}
		return next, true
	}
	return i, false
}

// varRef parses ${NAME} or $NAME at i, returning the name and the index
// of its last rune.
func varRef(runes []rune, i int) (name string, next int, ok bool) {
	if runes[i+1] == '{' {
		arg, end, found := bracedArg(runes, i)
		if !found {
			return "", i, false
		}
		// Only plain names; anything with shell operators in it is a
		// construct this expander deliberately does not implement.
		if !plainName(arg) {
			return "", i, false
		}
		return arg, end, true
	}
	end := i + 1
	for end < len(runes) && isNameRune(runes[end]) {
		end++
	}
	if end == i+1 {
		return "", i, false
	}
	return string(runes[i+1 : end]), end - 1, true
}

// bracedArg parses {arg} immediately after position i, returning the arg
// and the index of the closing brace.
func bracedArg(runes []rune, i int) (arg string, next int, ok bool) {
	if i+1 >= len(runes) || runes[i+1] != '{' {
		return "", i, false
	}
	for j := i + 2; j < len(runes); j++ {
		if runes[j] == '}' {
			return string(runes[i+2 : j]), j, true
		}
	}
	return "", i, false
}

func isNameRune(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func plainName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isNameRune(r) {
			return false
		}
	}
	return true
}

// decodeEscapes resolves the \u, \U and \x forms that presets use to
// spell glyphs they would rather not paste literally — the powerline
// separators arrive as ''. Unrecognized sequences are left alone.
func decodeEscapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '\\' || i+1 >= len(runes) {
			b.WriteRune(runes[i])
			continue
		}
		width := 0
		switch runes[i+1] {
		case 'u':
			width = 4
		case 'U':
			width = 8
		case 'x':
			width = 2
		default:
			// Simple C escapes; anything else keeps its backslash.
			if r, ok := simpleEscape(runes[i+1]); ok {
				b.WriteRune(r)
				i++
				continue
			}
			b.WriteRune(runes[i])
			continue
		}
		digits, n := hexRun(runes, i+2, width)
		if n == 0 {
			b.WriteRune(runes[i])
			continue
		}
		v, err := strconv.ParseUint(digits, 16, 32)
		if err != nil {
			b.WriteRune(runes[i])
			continue
		}
		b.WriteRune(rune(v))
		i += 1 + n
	}
	return b.String()
}

// hexRun reads up to max hex digits starting at i.
func hexRun(runes []rune, i, max int) (string, int) {
	var b strings.Builder
	n := 0
	for ; i < len(runes) && n < max; i++ {
		r := runes[i]
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String(), n
}

func simpleEscape(r rune) (rune, bool) {
	switch r {
	case 'n':
		return '\n', true
	case 't':
		return '\t', true
	case 'r':
		return '\r', true
	case 'e':
		return 27, true
	case '\\':
		return '\\', true
	}
	return 0, false
}
