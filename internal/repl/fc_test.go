package repl

import "testing"

// fcListRange is where the arithmetic lives, and the awkward cases are
// awkward to reach through a pty: an empty history, a range that runs
// off either end, and the negative form people use to mean "the last
// few".
//
// Positions here are 0-based, one below the numbers `fc -l` prints.
func TestFCListRange(t *testing.T) {
	t.Parallel()

	// Twenty entries, none of them the fc line itself, so lastHist and
	// realLast are the same — the case a unit test can state.
	entries := make([]string, 20)
	for i := range entries {
		entries[i] = "cmd"
	}
	const realLast, lastHist = 19, 19

	for _, tc := range []struct {
		name        string
		args        []string
		first, last int
		reverse     bool
	}{
		{name: "bare lists the last 16", first: 4, last: 19},
		{name: "one operand runs to the end", args: []string{"5"}, first: 4, last: 19},
		{name: "two operands bound both ends", args: []string{"5", "10"}, first: 4, last: 9},
		{name: "negative counts back from newest", args: []string{"-3"}, first: 17, last: 19},
		// Anything outside the list switches to the whole list — measured
		// against bash 5.3, which lists all twenty entries for `fc -l 25`,
		// not a default window and not an error. The old clamp ran before
		// the reversed-pair swap, so `fc -l 4` over two entries carried 4
		// past the ceiling check, indexed entries[3], and panicked —
		// history.tests lost 368 of its 410 lines to that one crash (#277).
		{name: "below the start lists everything", args: []string{"-100"}, first: 0, last: 19},
		{name: "past the end lists everything", args: []string{"1", "999"}, first: 0, last: 19},
		{name: "start past the end lists everything", args: []string{"30"}, first: 0, last: 19},
		{name: "whole range past the end lists everything", args: []string{"25", "30"}, first: 0, last: 19},
		// A reversed pair keeps its order — `fc -l 3 1` prints 3, 2, 1 in
		// bash, so first > last is a listing direction, not a mistake to
		// normalize away.
		{name: "reversed range keeps its order", args: []string{"10", "5"}, first: 4, last: 9, reverse: true},
		// Zero resolves to the newest entry, not to out-of-range.
		{name: "zero means the newest entry", args: []string{"0"}, first: 19, last: 19},
		{name: "zero to two lists newest-first", args: []string{"0", "18"}, first: 17, last: 19, reverse: true},
		// Anything past the second operand is ignored rather than refused,
		// which is bash's own reading of `fc -l one=two three=four 502`
		// (#711, history.tests).
		{name: "a third operand is ignored", args: []string{"5", "10", "99"}, first: 4, last: 9},
	} {
		first, last, reverse, kind := fcListRange(tc.args, realLast, lastHist, entries, 0)
		if kind != fcNumOK {
			t.Errorf("%s: unresolved (%v)", tc.name, kind)
			continue
		}
		if first != tc.first || last != tc.last || reverse != tc.reverse {
			t.Errorf("%s: got %d..%d reverse=%v, want %d..%d reverse=%v",
				tc.name, first, last, reverse, tc.first, tc.last, tc.reverse)
		}
	}

	// A specification that is not a number is a prefix search, and one
	// nothing starts with is "no command found" rather than a position.
	if _, _, _, kind := fcListRange([]string{"abc"}, realLast, lastHist, entries, 0); kind != fcNumNotFound {
		t.Errorf("a prefix nothing matches resolved to %v, want fcNumNotFound", kind)
	}
	if _, _, _, kind := fcListRange([]string{"cm"}, realLast, lastHist, entries, 0); kind != fcNumOK {
		t.Errorf("a prefix that matches resolved to %v, want fcNumOK", kind)
	}
}

// A history shorter than the default window must not produce a range
// starting below the first entry.
func TestFCListRangeShortHistory(t *testing.T) {
	t.Parallel()

	entries := []string{"a", "b", "c"}
	first, last, reverse, kind := fcListRange(nil, 2, 2, entries, 0)
	if kind != fcNumOK {
		t.Fatalf("unresolved: %v", kind)
	}
	if first != 0 || last != 2 || reverse {
		t.Errorf("short history = %d..%d reverse=%v, want 0..2 reverse=false", first, last, reverse)
	}
}

// `-0` and `0` are different specifications and the difference is not
// cosmetic: `fc -l -0` names the fc line itself, which nothing else can
// reach, while `fc -0` — the editor form — is not a position at all.
// history5.sub tests both by name.
func TestFCHistNumZero(t *testing.T) {
	t.Parallel()

	entries := []string{"a", "b", "c"}
	const realLast, base = 3, 0 // one more entry than `entries`: the fc line

	if got, kind := fcHistNum("0", true, entries, realLast, base, true, false); kind != fcNumOK || got != 2 {
		t.Errorf("`0` = %d (%v), want 2", got, kind)
	}
	if got, kind := fcHistNum("-0", true, entries, realLast, base, true, false); kind != fcNumOK || got != realLast {
		t.Errorf("listing `-0` = %d (%v), want %d", got, kind, realLast)
	}
	if _, kind := fcHistNum("-0", true, entries, realLast, base, false, false); kind != fcNumInvalid {
		t.Errorf("editing `-0` = %v, want fcNumInvalid", kind)
	}
}
