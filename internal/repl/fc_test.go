package repl

import "testing"

// fcRange is where the arithmetic lives, and the awkward cases are
// awkward to reach through a pty: an empty history, a range that runs
// off either end, and the negative form people use to mean "the last
// few".
func TestFCRange(t *testing.T) {
	t.Parallel()

	const n = 20
	for _, tc := range []struct {
		name        string
		args        []string
		first, last int
	}{
		{"bare lists the last 16", nil, 5, 20},
		{"one operand runs to the end", []string{"5"}, 5, 20},
		{"two operands bound both ends", []string{"5", "10"}, 5, 10},
		{"negative counts back from newest", []string{"-3"}, 18, 20},
		// Anything outside [1, n] switches to the whole list — measured
		// against bash 5.3, which lists all twenty entries for
		// `fc -l 25`, not a default window and not an error. The old
		// clamp ran before the reversed-pair swap, so `fc -l 4` over two
		// entries carried 4 past the ceiling check, indexed entries[3],
		// and panicked — history.tests lost 368 of its 410 lines to that
		// one crash (#277).
		{"below the start lists everything", []string{"-100"}, 1, 20},
		{"past the end lists everything", []string{"1", "999"}, 1, 20},
		{"start past the end lists everything", []string{"30"}, 1, 20},
		{"whole range past the end lists everything", []string{"25", "30"}, 1, 20},
		// A reversed pair keeps its order — `fc -l 3 1` prints 3, 2, 1
		// in bash, so first > last is a listing direction, not a mistake
		// to normalize away.
		{"reversed range keeps its order", []string{"10", "5"}, 10, 5},
		// Zero resolves to the newest entry, not to out-of-range.
		{"zero means the newest entry", []string{"0"}, 20, 20},
		{"zero to two lists newest-first", []string{"0", "18"}, 20, 18},
	} {
		first, last, err := fcRange(tc.args, n)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if first != tc.first || last != tc.last {
			t.Errorf("%s: got %d..%d, want %d..%d", tc.name, first, last, tc.first, tc.last)
		}
	}

	if _, _, err := fcRange([]string{"abc"}, n); err == nil {
		t.Error("a non-numeric position was accepted")
	}
	if _, _, err := fcRange([]string{"1", "2", "3"}, n); err == nil {
		t.Error("three operands were accepted")
	}
}

// A history shorter than the default window must not produce a range
// starting below the first entry.
func TestFCRangeShortHistory(t *testing.T) {
	t.Parallel()

	first, last, err := fcRange(nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || last != 3 {
		t.Errorf("short history = %d..%d, want 1..3", first, last)
	}
}
