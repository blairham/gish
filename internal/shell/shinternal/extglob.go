package shinternal

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/blairham/koi-shell/internal/shell/pattern"
)

// This file implements a backtracking matcher for extended glob patterns
// containing !(...) anywhere (#373). Go's regexp is RE2 and has no
// lookahead, so !(...) cannot become a regular expression once anything
// composes with it — the old fixed-prefix/suffix special case covered
// only the bare shape, and extglob2.tests composes it with suffixes,
// nests it inside *(...), and doubles it as !(*.*).!(*.*). The grammar
// here is the component grammar bash's fnmatch runs: literals, ?, *,
// bracket expressions, and the five group operators over |-separated
// alternatives. Matching memoizes (node, position) pairs, so the
// backtracking stays quadratic per group rather than exponential.

type extNode any

type (
	extLit   struct{ r rune }
	extAny   struct{}
	extStar  struct{}
	extClass struct{ rx *regexp.Regexp }
	extGroup struct {
		op   byte // '@', '+', '*', '?', '!'
		alts [][]extNode
	}
)

// extGlobMatcher parses pat and returns an anchored matcher, honoring
// NoGlobCase, Filenames, and GlobLeadingDot from mode.
func extGlobMatcher(pat string, mode pattern.Mode) (func(string) bool, error) {
	nodes, rest, err := parseExtSeq(pat, mode, false)
	if err != nil {
		return nil, err
	}
	if rest != "" {
		return nil, &pattern.SyntaxError{}
	}
	fold := mode&pattern.NoGlobCase != 0
	filenames := mode&pattern.Filenames != 0
	dotOK := mode&pattern.GlobLeadingDot != 0
	patDot := len(nodes) > 0 && func() bool {
		l, ok := nodes[0].(extLit)
		return ok && l.r == '.'
	}()
	return func(name string) bool {
		if filenames && !dotOK && strings.HasPrefix(name, ".") && !patDot {
			return false
		}
		if fold {
			name = strings.ToLower(name)
		}
		return matchExtSeq(nodes, []rune(name), filenames)
	}, nil
}

// parseExtSeq parses nodes until the end of the pattern, or — inside a
// group — until an unconsumed `|` or `)`.
func parseExtSeq(pat string, mode pattern.Mode, inGroup bool) ([]extNode, string, error) {
	fold := mode&pattern.NoGlobCase != 0
	var nodes []extNode
	lit := func(r rune) {
		if fold {
			r = unicode.ToLower(r)
		}
		nodes = append(nodes, extLit{r: r})
	}
	for len(pat) > 0 {
		r, size := utf8.DecodeRuneInString(pat)
		switch {
		case inGroup && (r == '|' || r == ')'):
			return nodes, pat, nil
		case r == '\\':
			pat = pat[size:]
			if pat == "" {
				lit('\\')
				continue
			}
			r, size = utf8.DecodeRuneInString(pat)
			lit(r)
			pat = pat[size:]
		case strings.ContainsRune("@+*?!", r) && strings.HasPrefix(pat[size:], "("):
			op := byte(r)
			rest := pat[size+1:]
			var alts [][]extNode
			for {
				alt, after, err := parseExtSeq(rest, mode, true)
				if err != nil {
					return nil, "", err
				}
				alts = append(alts, alt)
				if after == "" {
					return nil, "", &pattern.SyntaxError{}
				}
				if after[0] == '|' {
					rest = after[1:]
					continue
				}
				rest = after[1:] // consume ')'
				break
			}
			nodes = append(nodes, extGroup{op: op, alts: alts})
			pat = rest
		case r == '*':
			nodes = append(nodes, extStar{})
			pat = pat[size:]
		case r == '?':
			nodes = append(nodes, extAny{})
			pat = pat[size:]
		case r == '[':
			class, rest, ok := scanBracket(pat)
			if !ok {
				lit(r)
				pat = pat[size:]
				continue
			}
			classMode := pattern.EntireString
			if fold {
				classMode |= pattern.NoGlobCase
			}
			expr, err := pattern.Regexp(class, classMode)
			if err != nil {
				return nil, "", err
			}
			rx, err := regexp.Compile(expr)
			if err != nil {
				return nil, "", err
			}
			nodes = append(nodes, extClass{rx: rx})
			pat = rest
		default:
			lit(r)
			pat = pat[size:]
		}
	}
	if inGroup {
		return nil, "", &pattern.SyntaxError{}
	}
	return nodes, "", nil
}

// scanBracket finds the extent of a bracket expression starting at
// pat[0] == '[', honoring the leading-]-is-literal rule and [:class:]
// forms. ok is false for an unterminated expression.
func scanBracket(pat string) (class, rest string, ok bool) {
	i := 1
	if i < len(pat) && (pat[i] == '!' || pat[i] == '^') {
		i++
	}
	if i < len(pat) && pat[i] == ']' {
		i++ // a leading ] is literal
	}
	for i < len(pat) {
		switch {
		case pat[i] == '[' && i+1 < len(pat) && (pat[i+1] == ':' || pat[i+1] == '.' || pat[i+1] == '='):
			delim := pat[i+1]
			end := strings.Index(pat[i+2:], string(delim)+"]")
			if end < 0 {
				return "", "", false
			}
			i += 2 + end + 2
		case pat[i] == ']':
			return pat[:i+1], pat[i+1:], true
		default:
			i++
		}
	}
	return "", "", false
}

// matchExtSeq reports whether nodes match all of name.
func matchExtSeq(nodes []extNode, name []rune, filenames bool) bool {
	type key struct{ pi, ni int }
	memo := map[key]bool{}
	sep := func(r rune) bool { return filenames && r == '/' }
	var match func(pi, ni int) bool
	// altsMatch reports whether any alternative matches name[from:to].
	altsMatch := func(alts [][]extNode, from, to int) bool {
		for _, alt := range alts {
			if matchExtSeq(alt, name[from:to], filenames) {
				return true
			}
		}
		return false
	}
	match = func(pi, ni int) (res bool) {
		k := key{pi, ni}
		if v, ok := memo[k]; ok {
			return v
		}
		memo[k] = false // break repetition cycles conservatively
		defer func() { memo[k] = res }()
		if pi == len(nodes) {
			return ni == len(name)
		}
		switch n := nodes[pi].(type) {
		case extLit:
			return ni < len(name) && name[ni] == n.r && match(pi+1, ni+1)
		case extAny:
			return ni < len(name) && !sep(name[ni]) && match(pi+1, ni+1)
		case extClass:
			return ni < len(name) && !sep(name[ni]) &&
				n.rx.MatchString(string(name[ni])) && match(pi+1, ni+1)
		case extStar:
			for j := ni; j <= len(name); j++ {
				if match(pi+1, j) {
					return true
				}
				if j < len(name) && sep(name[j]) {
					break
				}
			}
			return false
		case extGroup:
			switch n.op {
			case '@':
				for j := ni; j <= len(name); j++ {
					if altsMatch(n.alts, ni, j) && match(pi+1, j) {
						return true
					}
				}
				return false
			case '?':
				if match(pi+1, ni) {
					return true
				}
				for j := ni; j <= len(name); j++ {
					if altsMatch(n.alts, ni, j) && match(pi+1, j) {
						return true
					}
				}
				return false
			case '!':
				// Any span the list does not match, the rest matching
				// after it — the whole reason this matcher exists.
				for j := ni; j <= len(name); j++ {
					if !altsMatch(n.alts, ni, j) && match(pi+1, j) {
						return true
					}
				}
				return false
			case '*', '+':
				// Repetitions: one occurrence spans ni..j, then the
				// group repeats from j. A zero-width occurrence would
				// loop; requiring progress matches bash.
				var rep func(from int) bool
				seen := map[int]bool{}
				rep = func(from int) bool {
					if seen[from] {
						return false
					}
					seen[from] = true
					if match(pi+1, from) {
						return true
					}
					for j := from + 1; j <= len(name); j++ {
						if altsMatch(n.alts, from, j) && rep(j) {
							return true
						}
					}
					return false
				}
				if n.op == '+' {
					for j := ni + 1; j <= len(name); j++ {
						if altsMatch(n.alts, ni, j) && rep(j) {
							return true
						}
					}
					return false
				}
				return rep(ni)
			}
		}
		return false
	}
	return match(0, 0)
}
