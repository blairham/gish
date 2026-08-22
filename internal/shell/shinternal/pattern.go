// Copyright (c) 2026, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package shinternal

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/blairham/koi-shell/internal/shell/pattern"
)

// ExtendedPatternMatcher returns a [regexp.Regexp.MatchString]-like function
// to support !(pattern-list) extended patterns where possible.
// It can be used instead of [pattern.Regexp] for narrow use cases.
func ExtendedPatternMatcher(pat string, mode pattern.Mode) (func(string) bool, error) {
	if mode&pattern.ExtendedOperators != 0 && mode&pattern.EntireString == 0 {
		// In the future we could try to support !(pattern) without matching
		// the entire input, ensuring we add enough test cases.
		panic("ExtendedOperators is only supported with EntireString")
	}

	// Collating symbols, equivalence classes, and bash's degrade rules
	// for invalid class names resolve before the pattern package reads
	// the expression (#374).
	//
	// The bracket rewrite has to stop at an extended glob group bash
	// cannot find the end of, because the unterminated `[` is *why* it
	// cannot: escaping it here would hand the pattern package a group
	// that closes, and !([*)* would glob where bash echoes it (#676).
	tail := -1
	if mode&pattern.ExtendedOperators != 0 {
		tail = pattern.ExtGlobLiteralTail(pat)
	}
	if tail >= 0 {
		pat = NormalizeBrackets(pat[:tail]) + quoteLiteral(pat[tail:])
	} else {
		pat = NormalizeBrackets(pat)
	}

	// In pathname expansion with dotglob off, a leading dot in a name
	// may only be consumed by a literal dot in the pattern. Two things
	// decide it, and this function owns both so that no caller has to
	// (#674):
	//
	//   - whether the pattern names the dot at all, which is bash's
	//     skipname and is a property of the pattern alone;
	//   - which dotfile then matches, which is a property of position,
	//     since every alternative of an extended glob group carries the
	//     rule independently: @(.a|*) matches .a and not .ab.
	//
	// The second cannot be an RE2 expression — the same missing
	// lookahead that sends !(...) to the backtracking matcher — so a
	// pattern holding a group runs that matcher too.
	dotRule := mode&pattern.Filenames != 0 && mode&pattern.GlobLeadingDot == 0
	// gate applies the first half: a pattern which does not name the dot
	// matches no dotfile at all. The pattern package suppresses them for
	// a leading * or ?, but a bracket class like [^a-c] still matched
	// them (#376), and an extglob group needs the pattern's structure
	// rather than its first byte (#674).
	gate := func(match func(string) bool) func(string) bool {
		if !dotRule || pattern.AllowsLeadingDot(pat, mode) {
			return match
		}
		return func(name string) bool {
			return !strings.HasPrefix(name, ".") && match(name)
		}
	}
	if dotRule && mode&pattern.ExtendedOperators != 0 && hasExtGlobGroup(pat) {
		match, err := extGlobMatcher(pat, mode)
		if err != nil {
			return nil, err
		}
		return gate(match), nil
	}

	// Extended pattern matching operators are always on outside of pathname expansion.
	expr, err := pattern.Regexp(pat, mode)
	if err != nil {
		// !(pattern-list) cannot become an RE2 expression once anything
		// composes with it — no lookahead — so those patterns run the
		// backtracking matcher instead (#373). It handles the group in
		// any position, nested and repeated included.
		var negErr *pattern.NegExtGlobError
		if !errors.As(err, &negErr) {
			return nil, err
		}
		match, err := extGlobMatcher(pat, mode)
		if err != nil {
			return nil, err
		}
		return gate(match), nil
	}
	// Compile, never MustCompile: pattern.Regexp can hand back an
	// expression Go's regexp rejects — an unclosed class inside an
	// extglob group, found as a shell-killing panic (#373) — and an
	// invalid pattern is the caller's literal-fallback case, not a
	// crash.
	rx, err := regexp.Compile(expr)
	if err != nil {
		return nil, &pattern.SyntaxError{}
	}
	return gate(rx.MatchString), nil
}

// quoteLiteral escapes every rune of s, so that the result is a pattern
// matching s and nothing else. [pattern.QuoteMeta] is not enough here:
// it leaves an extended glob operator's `(` alone, so an escaped `+`
// followed by a bare `(` would read as a group again. A rune that is
// already escaped keeps its one backslash, since quoting it a second
// time would turn a quoted `[` into a literal backslash.
func quoteLiteral(s string) string {
	var sb strings.Builder
	sb.Grow(2 * len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '\\' && i+size < len(s) {
			r2, size2 := utf8.DecodeRuneInString(s[i+size:])
			sb.WriteByte('\\')
			sb.WriteRune(r2)
			i += size + size2
			continue
		}
		sb.WriteByte('\\')
		sb.WriteRune(r)
		i += size
	}
	return sb.String()
}

// hasExtGlobGroup reports whether pat holds an extended glob group whose
// end bash can find. An unterminated one is not a group at all (#676),
// and neither is an operator character inside a bracket expression.
func hasExtGlobGroup(pat string) bool {
	for i := 0; i < len(pat); i++ {
		switch pat[i] {
		case '\\':
			i++
		case '[':
			if n := pattern.BracketEnd(pat[i:]); n > 0 {
				i += n - 1
			}
		case '!', '?', '*', '+', '@':
			if pattern.ExtGlobGroupEnd(pat, i) >= 0 {
				return true
			}
		}
	}
	return false
}
