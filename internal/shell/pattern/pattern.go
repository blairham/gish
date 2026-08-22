// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

// Package pattern allows working with shell pattern matching notation, also
// known as wildcards or globbing.
//
// For reference, see
// https://pubs.opengroup.org/onlinepubs/9699919799/utilities/V3_chap02.html#tag_18_13.
package pattern

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Mode can be used to supply a number of options to the package's functions.
// Not all functions change their behavior with all of the options below.
type Mode uint

type SyntaxError struct {
	msg string
	err error
}

func (e SyntaxError) Error() string { return e.msg }

func (e SyntaxError) Unwrap() error { return e.err }

// NegExtGlobGroup represents the byte offset range of a single !(expr) group
// within a pattern string. Start is the offset of '!', End is one past ')'.
type NegExtGlobGroup struct {
	Start, End int
}

// NegExtGlobError is returned by [Regexp] when an extglob negation operator
// !(pattern-list) is encountered, as Go's [regexp] package does not support
// negative lookahead. Callers can handle this by negating the result of
// matching the inner pattern.
type NegExtGlobError struct {
	Groups []NegExtGlobGroup
}

func (e *NegExtGlobError) Error() string {
	return "extglob !(...) is not supported in this scenario"
}

// TODO(v4): flip NoGlobStar to be opt-in via GlobStar, matching bash
// TODO(v4): flip EntireString to be opt-out via PartialMatch, as EntireString causes subtle bugs when forgotten
// TODO(v4): rename NoGlobCase to CaseInsensitive for readability

const (
	Shortest          Mode = 1 << iota // prefer the shortest match.
	Filenames                          // "*" and "?" don't match slashes; only "**" does; only makes sense with EntireString too
	EntireString                       // match the entire string using ^$ delimiters
	NoGlobCase                         // do case-insensitive match (that is, use (?i) in the regexp); shopt "nocaseglob"
	NoGlobStar                         // do not support "**"; negated shopt "globstar"
	GlobLeadingDot                     // let wildcards match leading dots in filenames; shopt "dotglob"
	ExtendedOperators                  // support extended pattern matching operators; shopt "extglob" for pathname expansion
)

// Regexp turns a shell pattern into a regular expression that can be used with
// [regexp.Compile]. It will return an error if the input pattern was incorrect.
// Otherwise, the returned expression can be passed to [regexp.MustCompile].
//
// For example, Regexp(`foo*bar?`, true) returns `foo.*bar.`.
//
// Note that this function (and [QuoteMeta]) should not be directly used with file
// paths if Windows is supported, as the path separator on that platform is the
// same character as the escaping character for shell patterns.
func Regexp(pat string, mode Mode) (string, error) {
	// If there are no special pattern matching or regular expression characters,
	// and we don't need to insert extras for the modes affecting non-special characters,
	// we can directly return the input string as a short-cut.
	if mode&(EntireString|NoGlobCase) == 0 {
		needsEscaping := false
	noopLoop:
		for _, r := range pat {
			switch r {
			// including those that need escaping since they are
			// regular expression metacharacters
			case '*', '?', '[', '\\', '.', '+', '(', ')', '|',
				']', '{', '}', '^', '$':
				needsEscaping = true
				break noopLoop
			}
		}
		if !needsEscaping {
			return pat, nil
		}
	}
	var sb strings.Builder
	// Enable matching `\n` with the `.` metacharacter as globs match `\n`
	sb.WriteString(`(?s`)
	if mode&NoGlobCase != 0 {
		sb.WriteString(`i`)
	}
	if mode&Shortest != 0 {
		sb.WriteString(`U`)
	}
	sb.WriteString(`)`)
	if mode&EntireString != 0 {
		sb.WriteString(`^`)
	}
	sl := stringLexer{s: pat}
	var negGroups []NegExtGlobGroup
	for {
		if err := regexpNext(&sb, &sl, mode); err == io.EOF {
			break
		} else if err != nil {
			negErr, ok := err.(*NegExtGlobError)
			if !ok {
				return "", err
			}
			negGroups = append(negGroups, negErr.Groups...)
		}
	}
	if len(negGroups) > 0 {
		return "", &NegExtGlobError{Groups: negGroups}
	}
	if mode&EntireString != 0 {
		sb.WriteString(`$`)
	}
	return sb.String(), nil
}

// stringLexer helps us tokenize a pattern string.
// Note that we can use the null byte '\x00' to signal "no character" as shell strings cannot contain null bytes.
type stringLexer struct {
	s string
	i int
}

func (sl *stringLexer) next() rune {
	if sl.i >= len(sl.s) {
		return '\x00'
	}
	c, size := utf8.DecodeRuneInString(sl.s[sl.i:])
	sl.i += size
	return c
}

func (sl *stringLexer) last() rune {
	if sl.i < 2 {
		return '\x00'
	}
	c, _ := utf8.DecodeLastRuneInString(sl.s[:sl.i-1])
	return c
}

func (sl *stringLexer) peekNext() rune {
	if sl.i >= len(sl.s) {
		return '\x00'
	}
	c, _ := utf8.DecodeRuneInString(sl.s[sl.i:])
	return c
}

func (sl *stringLexer) peekRest() string {
	return sl.s[sl.i:]
}

// BracketEnd reports the index just past the ']' closing the bracket
// expression at s[0], which must be '[', or -1 when the expression is
// unterminated. The leading-']'-is-literal rule and the [:class:],
// [.symbol.] and [=class=] forms are all scanned as units.
func BracketEnd(s string) int {
	if len(s) == 0 || s[0] != '[' {
		return -1
	}
	i := 1
	if i < len(s) && (s[i] == '!' || s[i] == '^') {
		i++
	}
	if i < len(s) && s[i] == ']' {
		i++ // a leading ] is literal
	}
	for i < len(s) {
		switch {
		case s[i] == '[' && i+1 < len(s) && (s[i+1] == ':' || s[i+1] == '.' || s[i+1] == '='):
			delim := s[i+1]
			end := strings.Index(s[i+2:], string(delim)+"]")
			if end < 0 {
				return -1
			}
			i += 2 + end + 2
		case s[i] == ']':
			return i + 1
		default:
			i++
		}
	}
	return -1
}

// ExtGlobGroupEnd reports the index just past the ')' closing the
// extended glob group whose operator byte is at pat[i] and whose '('
// must follow it, or -1 when the group cannot be terminated. Bracket
// expressions are scanned as units, which is what lets an unterminated
// one swallow the ')' and make the group unreadable (#676), and nested
// parentheses are counted by depth.
func ExtGlobGroupEnd(pat string, i int) int {
	if i >= len(pat) || !strings.ContainsRune("!?*+@", rune(pat[i])) {
		return -1
	}
	if i+1 >= len(pat) || pat[i+1] != '(' {
		return -1
	}
	depth := 1
	for j := i + 2; j < len(pat); j++ {
		switch pat[j] {
		case '\\':
			j++
		case '[':
			n := BracketEnd(pat[j:])
			if n < 0 {
				return -1
			}
			j += n - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return -1
}

// ExtGlobLiteralTail returns the index of the first extended glob
// operator in pat whose group bash cannot find the end of, or -1 when
// every group is terminated. From that operator to the end of the
// pattern, bash reads text rather than pattern (#676).
func ExtGlobLiteralTail(pat string) int {
	for i := 0; i < len(pat); i++ {
		switch pat[i] {
		case '\\':
			i++
		case '[':
			if n := BracketEnd(pat[i:]); n > 0 {
				i += n - 1
			}
		case '!', '?', '*', '+', '@':
			if i+1 < len(pat) && pat[i+1] == '(' && ExtGlobGroupEnd(pat, i) < 0 {
				return i
			}
		}
	}
	return -1
}

// extGlobAlts splits an extended glob group's body on its top-level
// '|' separators, leaving nested groups and bracket expressions whole.
func extGlobAlts(body string) []string {
	var alts []string
	start, depth := 0, 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++
		case '[':
			if n := BracketEnd(body[i:]); n > 0 {
				i += n - 1
			}
		case '(':
			depth++
		case ')':
			depth--
		case '|':
			if depth == 0 {
				alts = append(alts, body[start:i])
				start = i + 1
			}
		}
	}
	return append(alts, body[start:])
}

// AllowsLeadingDot reports whether pat may match a filename which
// begins with a dot while dotglob is off. This is bash's skipname rule,
// measured against 5.3 rather than derived: a pattern names the dot
// with a literal '.', and an extended glob group names it when any one
// of its alternatives does — recursively, so @(x|@(.a)) counts. Two
// asymmetries carry the rest. A negation never names it, whatever it
// holds: !(.foo) answers bar alone. And only the two operators that can
// match nothing, *( and ?(, hand the question on to the pattern after
// the group — *(bar).foo and ?(bar).foo match .foo where @(bar).foo,
// +(bar).foo and !(bar).foo match nothing at all.
//
// The rule is deliberately independent of the name: whether a *given*
// dotfile matches is then decided per position by the matcher, since
// each alternative of a group carries the rule on its own (#674).
func AllowsLeadingDot(pat string, mode Mode) bool {
	if strings.HasPrefix(pat, ".") || strings.HasPrefix(pat, `\.`) {
		return true
	}
	if mode&ExtendedOperators == 0 {
		return false
	}
	end := ExtGlobGroupEnd(pat, 0)
	if end < 0 {
		return false
	}
	if pat[0] == '!' {
		return false
	}
	for _, alt := range extGlobAlts(pat[2 : end-1]) {
		if AllowsLeadingDot(alt, mode) {
			return true
		}
	}
	if (pat[0] == '*' || pat[0] == '?') && end < len(pat) {
		return AllowsLeadingDot(pat[end:], mode)
	}
	return false
}

func regexpNext(sb *strings.Builder, sl *stringLexer, mode Mode) error {
	c := sl.next()
	if mode&ExtendedOperators != 0 {
		// Handle extended pattern matching operators separately,
		// given that they can be one of many two-character prefixes.
		// Note that we recurse into the same function in a loop,
		// as each of the patterns in the list separated by '|' is a regular pattern.
		switch op := c; op {
		case '!', '?', '*', '+', '@':
			if sl.peekNext() != '(' {
				break
			}
			start := sl.i - 1 // position of the operator
			if ExtGlobGroupEnd(sl.s, start) < 0 {
				// A group bash cannot find the end of is not a
				// group, and the operator does not simply become a
				// literal either: everything from it to the end of
				// the pattern is text, metacharacters and
				// backslashes included (#676). Measured against 5.3
				// — +(a|b[)* matches only itself, so the trailing
				// * is no longer a wildcard, while a wildcard
				// *before* the operator still is.
				sb.WriteString(regexp.QuoteMeta(sl.s[start:]))
				sl.i = len(sl.s)
				return nil
			}
			sb.WriteRune(sl.next()) // (
		nestedLoop:
			for {
				switch sl.peekNext() {
				case ')':
					break nestedLoop
				case '|':
					// extended operators support a list of "or" separated expressions
					sb.WriteRune(sl.next())
					continue
				}
				if err := regexpNext(sb, sl, mode); err == io.EOF {
					break
				} else if err != nil {
					return err
				}
			}
			sb.WriteRune(sl.next()) // )
			if op == '!' {
				return &NegExtGlobError{Groups: []NegExtGlobGroup{{Start: start, End: sl.i}}}
			}
			if op != '@' {
				// @( is [syntax.GlobOne] for matching once; no suffix needed
				sb.WriteRune(op)
			}
			return nil
		}
	}
	switch c {
	case '\x00':
		return io.EOF
	case '*':
		if mode&Filenames == 0 {
			// * - matches anything when not in filename mode
			sb.WriteString(`.*`)
			break
		}
		// "**" only acts as globstar if it is alone as a path element.
		singleBefore := sl.i == 1 || sl.last() == '/'
		// A second "*" that opens an extended glob group is that
		// group's operator, not the other half of a "**": bash reads
		// ab**(e|f) as ab, then *, then *(e|f), and answers
		// "abc abef" where koi answered the literal word (#677).
		extGlobNext := mode&ExtendedOperators != 0 && ExtGlobGroupEnd(sl.s, sl.i) >= 0
		if sl.peekNext() == '*' && !extGlobNext {
			sl.i++
			singleAfter := sl.i == len(sl.s) || sl.peekNext() == '/'
			if mode&NoGlobStar == 0 && singleBefore && singleAfter {
				// ** - match any number of slashes or "*" path elements
				slashSuffix := sl.peekNext() == '/'
				if slashSuffix {
					// **/ - like "**" but requiring a trailing slash when matching
					sl.i++
					// wrap the expression to ensure that any match has a slash suffix
					sb.WriteString(`(`)
				}
				if mode&GlobLeadingDot == 0 {
					sb.WriteString(`(/|[^/.][^/]*)*`)
				} else {
					// with GlobLeadingDot (dotglob), match anything at all
					sb.WriteString(`.*`)
				}
				if slashSuffix {
					sb.WriteString(`/)?`)
				}
				break
			}
			// foo**, **bar, or NoGlobStar - behaves like "*" below
		}
		// * - matches anything except slashes and leading dots
		if singleBefore && mode&GlobLeadingDot == 0 {
			sb.WriteString(`([^/.][^/]*)?`)
		} else {
			// with GlobLeadingDot (dotglob), match anything except slashes
			sb.WriteString(`[^/]*`)
		}
	case '?':
		if mode&Filenames != 0 {
			sb.WriteString(`[^/]`)
		} else {
			sb.WriteByte('.')
		}
	case '\\':
		c = sl.next()
		if c == '\x00' {
			return &SyntaxError{msg: `\ at end of pattern`}
		}
		sb.WriteString(regexp.QuoteMeta(string(c)))
	case '[':
		lit := sl.i // to reparse from, if the bracket turns out to be literal
		filenames := mode&Filenames != 0
		// Build the bracket expression separately; in Filenames mode, one
		// which could match a slash must be emitted literally instead.
		var bsb strings.Builder
		bsb.WriteByte('[')
		hasSlash := false
		var deferredErr error // reported only if the bracket expression closes
		var classErr error    // reported even if the bracket is unmatched
		// Like Bash, an unmatched "[" is a literal; emit it and reparse
		// the rest of the pattern.
		literalBracket := func() error {
			sl.i = lit
			sb.WriteString(`\[`)
			return nil
		}
		if c = sl.next(); c == '\x00' {
			return literalBracket()
		}
		switch c {
		case '!', '^':
			bsb.WriteByte('^')
			if c = sl.next(); c == '\x00' {
				return literalBracket()
			}
		}
		if c == ']' {
			bsb.WriteByte(']')
			if c = sl.next(); c == '\x00' {
				return literalBracket()
			}
		}
		for {
			switch c {
			case '\x00':
				// Bash is inconsistent about invalid character classes
				// in an unmatched bracket: `case` treats the whole
				// bracket as literal, but ${a//[[:} refuses to match.
				// We follow the latter, keeping the error.
				if classErr != nil {
					return classErr
				}
				return literalBracket()
			case '\\':
				// An escaped character matches itself; quote it so that
				// the regexp doesn't give it a special meaning, such as
				// \0 being an octal escape or \d a character class.
				switch c = sl.next(); {
				case c == '\x00':
					continue // handled by the case above
				case c == '-':
					// regexp.QuoteMeta does not escape '-', which would
					// form a range inside a bracket expression.
					bsb.WriteString(`\-`)
				case c > utf8.RuneSelf:
					bsb.WriteRune(c)
				default:
					if filenames && c == '/' {
						hasSlash = true
					}
					bsb.WriteString(regexp.QuoteMeta(string(c)))
				}
			case '-':
				bsb.WriteByte('-')
				start := sl.last()
				end := sl.peekNext()
				// TODO: what about overlapping ranges, like: [a--z]
				if end != ']' && start > end && deferredErr == nil {
					deferredErr = &SyntaxError{msg: fmt.Sprintf("invalid range: %c-%c", start, end)}
				}
			case ']':
				if hasSlash {
					// Bracket expressions can't match slashes in filename
					// patterns; emit the whole expression literally.
					sb.WriteString(regexp.QuoteMeta(sl.s[lit-1 : sl.i]))
					return nil
				}
				if deferredErr != nil {
					return deferredErr
				}
				bsb.WriteByte(']')
				sb.WriteString(bsb.String())
				return nil
			case '[':
				rest := sl.peekRest()
				n, err := charClass(rest)
				if err != nil {
					if classErr == nil {
						classErr = &SyntaxError{msg: "charClass invalid", err: err}
					}
					if deferredErr == nil {
						deferredErr = classErr
					}
				}
				bsb.WriteByte('[')
				if n > 0 {
					if filenames && strings.Contains(rest[:n], "/") {
						hasSlash = true
					}
					bsb.WriteString(rest[:n])
					sl.i += n
				}
			default:
				if filenames && c == '/' {
					hasSlash = true
				}
				bsb.WriteRune(c)
			}
			c = sl.next()
		}
	default:
		if c > utf8.RuneSelf {
			sb.WriteRune(c)
		} else {
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return nil
}

// charClass returns the length in bytes of the bracket expression element
// starting just after an opening "[" in s, such as ":alpha:]" for the
// character class [:alpha:]. Elements which are well formed but invalid or
// unsupported, such as collating symbols, return their length with an error.
func charClass(s string) (int, error) {
	if len(s) > 0 && (s[0] == '.' || s[0] == '=') {
		sep := ".]"
		if s[0] == '=' {
			sep = "=]"
		}
		name, _, ok := strings.Cut(s[1:], sep)
		if !ok {
			return 0, fmt.Errorf("collating features not available")
		}
		return len(name) + 3, fmt.Errorf("collating features not available")
	}
	name, ok := strings.CutPrefix(s, ":")
	if !ok {
		return 0, nil
	}
	name, _, ok = strings.Cut(name, ":]")
	if !ok {
		return 0, fmt.Errorf("[[: was not matched with a closing :]")
	}
	switch name {
	case "alnum", "alpha", "ascii", "blank", "cntrl", "digit", "graph",
		"lower", "print", "punct", "space", "upper", "word", "xdigit":
	default:
		return len(name) + 3, fmt.Errorf("invalid character class: %q", name)
	}
	return len(name) + 3, nil
}

// HasMeta returns whether a string contains any unescaped pattern
// metacharacters: '*', '?', or '[' followed by a matching ']'. When the
// function returns false, the given pattern can only match at most one string.
//
// For example, HasMeta(`foo\*bar`) returns false, but HasMeta(`foo*bar`)
// returns true.
//
// This can be useful to avoid extra work, like [Regexp]. Note that this
// function cannot be used to avoid [QuoteMeta], as backslashes are quoted by
// that function but ignored here.
//
// The [Mode] parameter is unused, and will be removed in v4.
func HasMeta(pat string, mode Mode) bool {
	openBracket := false
	for i := 0; i < len(pat); i++ {
		switch pat[i] {
		case '\\':
			i++
		case '*', '?':
			return true
		case '[':
			openBracket = true
		case ']':
			// Like Bash, an unmatched '[' is a literal,
			// so it can only match one string.
			if openBracket {
				return true
			}
		}
	}
	return false
}

// QuoteMeta returns a string that quotes all pattern metacharacters in the
// given text. The returned string is a pattern that matches the literal text.
//
// For example, QuoteMeta(`foo*bar?`) returns `foo\*bar\?`.
//
// The [Mode] parameter is unused, and will be removed in v4.
func QuoteMeta(pat string, mode Mode) string {
	needsEscaping := false
loop:
	for _, r := range pat {
		switch r {
		case '*', '?', '[', '\\':
			needsEscaping = true
			break loop
		}
	}
	if !needsEscaping { // short-cut without a string copy
		return pat
	}
	var sb strings.Builder
	for _, r := range pat {
		switch r {
		case '*', '?', '[', '\\':
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
