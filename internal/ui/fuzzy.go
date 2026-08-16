package ui

import "strings"

// Fuzzy subsequence matching, fzf-shaped (#100): every query rune must
// appear in order, and the score rewards the things that make a match
// feel right — consecutive runs, word starts, and matching early.
//
// Deliberately not a dependency: the whole algorithm is short, and a
// picker that ships inside the shell should not carry a matcher whose
// behavior we cannot tune.

// Match is a scored candidate with the positions that matched, so a
// renderer can highlight them.
type Match struct {
	Index     int // index into the original candidate slice
	Score     int
	Positions []int // rune offsets in the candidate that matched
}

// scoring weights, in the order a user notices them.
const (
	scoreMatch       = 16 // any matched rune
	bonusConsecutive = 8  // adjacent to the previous match
	bonusWordStart   = 12 // after a separator: a "word" the user aimed at
	bonusFirstRune   = 8  // the very start of the candidate
	penaltyGap       = 1  // per skipped rune, so earlier matches win
)

// FuzzyFilter scores candidates against query, returning matches
// best-first. An empty query matches everything, in the input order —
// a picker with no typing should show the list as given.
func FuzzyFilter(candidates []string, query string) []Match {
	if query == "" {
		out := make([]Match, len(candidates))
		for i := range candidates {
			out[i] = Match{Index: i}
		}
		return out
	}
	q := []rune(strings.ToLower(query))
	var out []Match
	for i, c := range candidates {
		if m, ok := scoreOne([]rune(c), q); ok {
			m.Index = i
			out = append(out, m)
		}
	}
	// Stable insertion by score keeps equal scores in input order,
	// which for history means "most recent first" stays meaningful.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Score > out[j-1].Score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// scoreOne walks the candidate greedily, taking each query rune at its
// first opportunity.
func scoreOne(candidate, query []rune) (Match, bool) {
	var m Match
	qi := 0
	prevMatch := -2
	for ci := 0; ci < len(candidate) && qi < len(query); ci++ {
		if toLowerRune(candidate[ci]) != query[qi] {
			continue
		}
		score := scoreMatch
		switch {
		case ci == 0:
			score += bonusFirstRune + bonusWordStart
		case prevMatch == ci-1:
			score += bonusConsecutive
		case isSeparator(candidate[ci-1]):
			score += bonusWordStart
		}
		m.Score += score
		m.Positions = append(m.Positions, ci)
		prevMatch = ci
		qi++
	}
	if qi < len(query) {
		return Match{}, false // not a subsequence
	}
	// Prefer matches that finish early: a query buried at the end of a
	// long line is a worse hit than the same query near the start.
	if len(m.Positions) > 0 {
		m.Score -= m.Positions[0] * penaltyGap
	}
	return m, true
}

func isSeparator(r rune) bool {
	switch r {
	case ' ', '\t', '/', '-', '_', '.', ':', ',', ';', '=', '@':
		return true
	}
	return false
}

func toLowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}
