// Copyright (c) 2026, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package shinternal

import (
	"errors"
	"regexp"

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
	pat = NormalizeBrackets(pat)

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
		return extGlobMatcher(pat, mode)
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
	return rx.MatchString, nil
}
