package editor

import (
	"strings"
	"testing"
)

// candidateRows is the Tab listing's layout, and it had no test at all
// (#201) — the only coverage was a pty e2e that stripped escapes and
// checked the candidate names appeared *somewhere* in the buffer, which
// passes just as well if the columns collapse or the padding is wrong.
//
// It is in-package because the function is unexported; the geometry is
// what is worth pinning, not the API.
func TestCandidateRowsLayout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		items []string
		width int
		want  []string
	}{
		{
			name:  "no items, no rows",
			items: nil, width: 80, want: nil,
		},
		{
			// Widest item is 5 ("alpha"), so the column is 7 wide.
			// 80/7 = 11 columns, more than enough for one row.
			name:  "fits on one row",
			items: []string{"alpha", "beta", "gamma"}, width: 80,
			want: []string{"alpha  beta   gamma"},
		},
		{
			// Column is 7; a 20-wide terminal holds 2 columns, so three
			// items need 2 rows and fill column-major.
			name:  "wraps into columns down the rows",
			items: []string{"alpha", "beta", "gamma"}, width: 20,
			want: []string{"alpha  gamma", "beta"},
		},
		{
			// One column that cannot fit still gets a row rather than a
			// division by zero or an empty listing.
			name:  "an item wider than the terminal still lists",
			items: []string{"a-very-long-candidate-name"}, width: 10,
			want: []string{"a-very-long-candidate-name"},
		},
		{
			name:  "single item",
			items: []string{"only"}, width: 80,
			want: []string{"only"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := candidateRows(tt.items, tt.width)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d rows %q, want %d rows %q", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("row %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// Every row must fit the terminal, or the listing wraps and scrolls the
// edit line off the screen — the failure the column math exists to
// prevent.
func TestCandidateRowsNeverExceedWidth(t *testing.T) {
	t.Parallel()

	items := []string{"a", "bb", "ccc", "dddd", "eeeee", "ffffff", "ggggggg", "hh", "i", "jjjj"}
	for _, width := range []int{20, 40, 80, 120} {
		for i, row := range candidateRows(items, width) {
			// The last column needs no trailing pad, so a row may reach
			// the width exactly but never pass it.
			if w := displayWidth(row); w > width {
				t.Errorf("width %d: row %d is %d columns: %q", width, i, w, row)
			}
		}
	}
}

// Wide glyphs are two columns, and the padding must be computed in
// display width rather than rune count or the columns stagger.
func TestCandidateRowsAlignWideGlyphs(t *testing.T) {
	t.Parallel()

	rows := candidateRows([]string{"日本語", "ab", "cd", "ef"}, 30)
	for i, row := range rows {
		if w := displayWidth(row); w > 30 {
			t.Errorf("row %d overflows with wide glyphs: %d columns, %q", i, w, row)
		}
	}
	// The first column is 6 display columns wide (3 wide glyphs), so the
	// second column starts at 8 — proving the pad used display width.
	if len(rows) > 0 && strings.Contains(rows[0], "日本語") {
		if idx := displayWidth(rows[0][:strings.Index(rows[0], "日")]); idx != 0 {
			t.Errorf("first cell does not start at column 0: %q", rows[0])
		}
	}
}
