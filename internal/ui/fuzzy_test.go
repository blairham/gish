package ui

import "testing"

func TestFuzzyFilterOrdersByRelevance(t *testing.T) {
	t.Parallel()

	candidates := []string{
		"git commit --amend",
		"gzip -9 backup.tar",
		"go generate ./...",
		"grep -r TODO .",
		"unrelated line",
	}
	got := FuzzyFilter(candidates, "gc")
	if len(got) == 0 {
		t.Fatal("no matches for a query that is clearly a subsequence")
	}
	// "git commit" wins: both runes start words.
	if candidates[got[0].Index] != "git commit --amend" {
		t.Errorf("best match = %q, want the word-start hit", candidates[got[0].Index])
	}
	// Every result really is a subsequence match.
	for _, m := range got {
		if len(m.Positions) != 2 {
			t.Errorf("%q matched %d positions", candidates[m.Index], len(m.Positions))
		}
	}
}

func TestFuzzyFilterScoringPreferences(t *testing.T) {
	t.Parallel()

	// Consecutive beats scattered.
	got := FuzzyFilter([]string{"x_a_b_c_y", "abc"}, "abc")
	if got[0].Index != 1 {
		t.Error("scattered match outranked the consecutive one")
	}
	// Earlier beats later, all else equal.
	got = FuzzyFilter([]string{"zzzzzzzzzz target", "target zzzzzzzzzz"}, "target")
	if got[0].Index != 1 {
		t.Error("late match outranked the early one")
	}
	// Case-insensitive.
	if len(FuzzyFilter([]string{"MakeFile"}, "makefile")) != 1 {
		t.Error("case sensitivity crept in")
	}
}

func TestFuzzyFilterEdgeCases(t *testing.T) {
	t.Parallel()

	all := []string{"one", "two", "three"}
	// An empty query keeps everything, in input order.
	got := FuzzyFilter(all, "")
	if len(got) != 3 || got[0].Index != 0 || got[2].Index != 2 {
		t.Errorf("empty query = %+v", got)
	}
	// A non-subsequence matches nothing.
	if len(FuzzyFilter(all, "xyz")) != 0 {
		t.Error("non-subsequence matched")
	}
	// Order matters: "eno" is not a subsequence of "one".
	if len(FuzzyFilter([]string{"one"}, "eno")) != 0 {
		t.Error("out-of-order query matched")
	}
	if len(FuzzyFilter(nil, "q")) != 0 {
		t.Error("nil candidates matched")
	}
}
