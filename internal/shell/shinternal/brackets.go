package shinternal

import "strings"

// This file rewrites bracket expressions into the plain subset the
// pattern package reads correctly (#374), following bash 5.3's measured
// rules:
//
//   - [.c.] and [=c=] with a single character resolve to that character,
//     early enough to serve as a range endpoint: [[.a.]-z] is a-z.
//   - A multi-character or unterminated collating symbol contributes
//     nothing (bash matches nothing on [[.ab.]]).
//   - [:name:] with a valid class name is kept; with an invalid name and
//     a closing :] it contributes nothing; without the closing :] the
//     bracket and colon are ordinary members ([[:alpha] is the set
//     { [ : a l p h }).

var posixClasses = map[string]bool{
	"alnum": true, "alpha": true, "ascii": true, "blank": true,
	"cntrl": true, "digit": true, "graph": true, "lower": true,
	"print": true, "punct": true, "space": true, "upper": true,
	"word": true, "xdigit": true,
}

// NormalizeBrackets rewrites every bracket expression in pat; text
// outside brackets passes through untouched.
func NormalizeBrackets(pat string) string {
	if !strings.Contains(pat, "[") {
		return pat
	}
	var sb strings.Builder
	for i := 0; i < len(pat); i++ {
		b := pat[i]
		switch b {
		case '\\':
			sb.WriteByte(b)
			if i+1 < len(pat) {
				i++
				sb.WriteByte(pat[i])
			}
		case '[':
			rewritten, consumed := normalizeBracketAt(pat[i:])
			if consumed == 0 {
				// A `[` that never closes is a literal `[`, and the
				// scan carries on from the next byte — so
				// `[[:alpha:]` is a literal bracket followed by a real
				// bracket expression, which is how bash reads it and
				// why it matches the file named `[a` (#468).
				//
				// It has to be *escaped* rather than written back as
				// it stands: left raw, the pattern package reads it as
				// opening a bracket again and swallows the expression
				// after it.
				sb.WriteString(`\[`)
				continue
			}
			sb.WriteString(rewritten)
			i += consumed - 1
		default:
			sb.WriteByte(b)
		}
	}
	return sb.String()
}

// normalizeBracketAt parses one bracket expression at s[0] == '[' and
// returns its rewritten form and how many input bytes it spans; a
// consumed of 0 means s does not start a bracket expression.
func normalizeBracketAt(s string) (string, int) {
	i := 1
	neg := false
	if i < len(s) && (s[i] == '!' || s[i] == '^') {
		neg = true
		i++
	}
	// Tokens: single characters (as strings), or kept class pieces like
	// [:alpha:]. Ranges are assembled afterwards, once collating symbols
	// have resolved to characters.
	var tokens []btoken
	first := true
	for i < len(s) {
		switch {
		case s[i] == ']' && !first:
			return emitBracket(neg, tokens), i + 1
		case strings.HasPrefix(s[i:], "[:"):
			end := strings.Index(s[i+2:], ":]")
			if end < 0 {
				// No closing :]: the bracket and colon are members.
				tokens = append(tokens, btoken{ch: '['}, btoken{ch: ':'})
				i += 2
				first = false
				continue
			}
			name := s[i+2 : i+2+end]
			if posixClasses[name] {
				tokens = append(tokens, btoken{piece: "[:" + name + ":]"})
			}
			// An invalid class contributes nothing.
			i += 2 + end + 2
			first = false
		case strings.HasPrefix(s[i:], "[.") || strings.HasPrefix(s[i:], "[="):
			delim := s[i+1]
			end := strings.Index(s[i+2:], string(delim)+"]")
			if end < 0 {
				tokens = append(tokens, btoken{ch: '['}, btoken{ch: delim})
				i += 2
				first = false
				continue
			}
			if sym := s[i+2 : i+2+end]; len(sym) == 1 {
				tokens = append(tokens, btoken{ch: sym[0]})
			}
			// A multi-character symbol contributes nothing.
			i += 2 + end + 2
			first = false
		case s[i] == '\\' && i+1 < len(s):
			tokens = append(tokens, btoken{ch: s[i+1]})
			i += 2
			first = false
		default:
			tokens = append(tokens, btoken{ch: s[i]})
			i++
			first = false
		}
	}
	return "", 0 // unterminated: not a bracket expression
}

// emitBracket rebuilds a bracket expression from its tokens, assembling
// x-y ranges and placing the bytes the class syntax gives meaning — a
// member ']' first, '-' last — where they read as ordinary members.
func emitBracket(neg bool, tokens []btoken) string {
	var pieces []string
	var chars []byte
	for j := 0; j < len(tokens); j++ {
		t := tokens[j]
		if t.piece != "" {
			pieces = append(pieces, t.piece)
			continue
		}
		if t.ch != '-' && j+2 < len(tokens) &&
			tokens[j+1].piece == "" && tokens[j+1].ch == '-' &&
			tokens[j+2].piece == "" && tokens[j+2].ch != '-' {
			pieces = append(pieces, string(t.ch)+"-"+string(tokens[j+2].ch))
			j += 2
			continue
		}
		chars = append(chars, t.ch)
	}
	var sb strings.Builder
	sb.WriteByte('[')
	if neg {
		sb.WriteByte('!')
	}
	rest := make([]byte, 0, len(chars))
	hasBracket, hasDash := false, false
	for _, c := range chars {
		switch c {
		case ']':
			hasBracket = true
		case '-':
			hasDash = true
		default:
			rest = append(rest, c)
		}
	}
	if hasBracket {
		sb.WriteByte(']')
	}
	for _, p := range pieces {
		sb.WriteString(p)
	}
	for _, c := range rest {
		if c == '[' || c == ':' || c == '.' || c == '=' || c == '\\' || c == '^' {
			// A member that could re-form [:…:] syntax — or read as an
			// escape or negation — is escaped rather than ordered
			// around.
			sb.WriteByte('\\')
		}
		sb.WriteByte(c)
	}
	if hasDash {
		sb.WriteByte('-')
	}
	if sb.Len() == 1 || (neg && sb.Len() == 2) {
		// Every member resolved to nothing: a set that matches no
		// character at all (or, negated, any character).
		if neg {
			return "?"
		}
		return "[^\\x00-\\x{10FFFF}]"
	}
	sb.WriteByte(']')
	return sb.String()
}

// btoken is one resolved bracket-expression member: a character, or a
// kept [:class:] piece.
type btoken struct {
	ch    byte
	piece string
}
