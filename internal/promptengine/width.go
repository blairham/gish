package promptengine

import (
	"strings"

	"github.com/rivo/uniseg"
)

// displayWidth is the on-screen width of s with ANSI escapes discounted.
//
// The layout needs this to fill the gap between the left and right sides
// of a line, and it has to be right: a prompt that miscounts by one
// wraps the terminal and smears the frame on every keystroke. Escapes
// are zero-width; everything else is measured in grapheme clusters, so
// an emoji or a combining sequence counts once, at its rendered width.
//
// internal/editor keeps its own equivalent fused into its render
// tokenizer; this is the standalone form, so the prompt engine does not
// depend on the editor.
func displayWidth(s string) int {
	total := 0
	for len(s) > 0 {
		if rest, ok := skipEscape(s); ok {
			s = rest
			continue
		}
		cluster, rest, w, _ := uniseg.FirstGraphemeClusterInString(s, -1)
		if cluster == "" {
			break
		}
		total += w
		s = rest
	}
	return total
}

// skipEscape consumes one ANSI escape sequence at the head of s,
// reporting whether it found one. It covers the two forms a prompt can
// contain: CSI sequences (colors, cursor moves) and OSC strings
// (hyperlinks, which the dir segment emits when configured).
func skipEscape(s string) (string, bool) {
	if !strings.HasPrefix(s, "\x1b") || len(s) < 2 {
		return s, false
	}
	switch s[1] {
	case '[': // CSI: parameters, then a final byte in @-~
		for i := 2; i < len(s); i++ {
			if s[i] >= '@' && s[i] <= '~' {
				return s[i+1:], true
			}
		}
		return "", true // unterminated: nothing measurable remains
	case ']': // OSC: terminated by BEL or ST, whichever comes first
		// Scanning for each terminator separately and preferring BEL
		// would let a BEL anywhere later in the string win over this
		// sequence's own ST, swallowing the text between them.
		for i := 2; i < len(s); i++ {
			if s[i] == '\a' {
				return s[i+1:], true
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return s[i+2:], true
			}
		}
		return "", true
	default: // two-byte escape
		return s[2:], true
	}
}
