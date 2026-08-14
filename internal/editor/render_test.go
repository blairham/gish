package editor

import (
	"strings"
	"testing"
)

func TestWrapLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		line  string
		width int
		want  []string
	}{
		{"empty", "", 10, []string{""}},
		{"fits", "hello", 10, []string{"hello"}},
		{"exact", "hello", 5, []string{"hello"}},
		{"wraps", "hello world", 5, []string{"hello", " worl", "d"}},
		{"wide runes", "ややや", 4, []string{"やや", "や"}}, // width-2 each
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := wrapLine(tt.line, tt.width)
			if len(got) != len(tt.want) {
				t.Fatalf("wrapLine() = %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("row %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFrameCursorPosition(t *testing.T) {
	t.Parallel()

	lines := []string{"gish$ echo hi", "> done"}

	// Cursor on line 0 at display col 12: inside the wrapped second row.
	rows, curRow, curCol := frame(lines, 10, 0, 12)
	if len(rows) != 3 { // 13-wide first line wraps into 2 rows + 1
		t.Fatalf("rows = %q", rows)
	}
	if curRow != 1 || curCol != 2 {
		t.Errorf("cursor = row %d col %d, want row 1 col 2", curRow, curCol)
	}

	// Cursor on line 1 at display col 2: below the wrapped first line.
	_, curRow, curCol = frame(lines, 10, 1, 2)
	if curRow != 2 || curCol != 2 {
		t.Errorf("cursor = row %d col %d, want row 2 col 2", curRow, curCol)
	}
}

func TestRendererDiffSkipsUnchangedRows(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	r := newRenderer(&out, 40)
	r.render([]string{"gish$ echo one", "> two"}, 0, 14)
	first := out.String()
	if !strings.Contains(first, "echo one") || !strings.Contains(first, "two") {
		t.Fatalf("first frame missing content: %q", first)
	}

	out.Reset()
	// Only the second line changes; the first row must not be rewritten.
	r.render([]string{"gish$ echo one", "> tvo"}, 1, 5)
	second := out.String()
	if strings.Contains(second, "echo one") {
		t.Errorf("unchanged row was rewritten: %q", second)
	}
	if !strings.Contains(second, "tvo") {
		t.Errorf("changed row not drawn: %q", second)
	}
}

func TestRendererClearsShrunkRegion(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	r := newRenderer(&out, 40)
	r.render([]string{"gish$ a", "> b", "> c"}, 2, 3)
	out.Reset()
	r.render([]string{"gish$ a"}, 0, 7)
	got := out.String()
	if !strings.Contains(got, "\x1b[J") {
		t.Errorf("shrinking frame did not erase below: %q", got)
	}
}

func TestRendererFinishMovesBelowRegion(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	r := newRenderer(&out, 40)
	r.render([]string{"gish$ a", "> b"}, 0, 7) // cursor on top row
	out.Reset()
	r.finish()
	got := out.String()
	if !strings.Contains(got, "\x1b[1B") {
		t.Errorf("finish did not move below region: %q", got)
	}
	if !strings.HasSuffix(got, "\r\n") {
		t.Errorf("finish did not end with newline: %q", got)
	}
	if r.rows != nil || r.curRow != 0 {
		t.Error("finish did not reset renderer state")
	}
}
