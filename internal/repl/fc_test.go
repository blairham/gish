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
		{"below the start clamps", []string{"-100"}, 1, 20},
		{"past the end clamps", []string{"1", "999"}, 1, 20},
		// A reversed range prints nothing if taken literally, which reads
		// as a broken command rather than as a mistake in the arguments.
		{"reversed range is normalized", []string{"10", "5"}, 5, 10},
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
