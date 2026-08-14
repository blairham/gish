package editor

import (
	"strings"
	"testing"
)

func TestDisplayWidthIgnoresANSI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		s    string
		want int
	}{
		{"plain", 5},
		{"\x1b[36mcolor\x1b[0m", 5},
		{"\x1b[2m╭─ \x1b[0m\x1b[36m~/x\x1b[0m", 6},
		{"や\x1b[31mや\x1b[0m", 4}, // wide runes still count
		{"", 0},
	}
	for _, tt := range tests {
		if got := displayWidth(tt.s); got != tt.want {
			t.Errorf("displayWidth(%q) = %d, want %d", tt.s, got, tt.want)
		}
	}
}

func TestWrapLineKeepsANSIAtomic(t *testing.T) {
	t.Parallel()

	// Five visible cells of colored text at width 3: the escape
	// sequences must never be split across rows.
	rows := wrapLine("\x1b[31mabcde\x1b[0m", 3)
	if len(rows) != 2 {
		t.Fatalf("rows = %q", rows)
	}
	for _, row := range rows {
		if strings.Count(row, "\x1b") > 0 && !strings.Contains(row, "m") {
			t.Errorf("escape split across rows: %q", rows)
		}
	}
	if got := displayWidth(rows[0]); got != 3 {
		t.Errorf("row 0 width = %d, want 3", got)
	}
	if got := displayWidth(rows[1]); got != 2 {
		t.Errorf("row 1 width = %d, want 2", got)
	}
}

func TestPromptParts(t *testing.T) {
	t.Parallel()

	banner, prefix := promptParts("line1\nline2\n❯ ")
	if len(banner) != 2 || banner[0] != "line1" || banner[1] != "line2" {
		t.Errorf("banner = %q", banner)
	}
	if prefix != "❯ " {
		t.Errorf("prefix = %q", prefix)
	}

	banner, prefix = promptParts("$ ")
	if len(banner) != 0 || prefix != "$ " {
		t.Errorf("single-line prompt: banner=%q prefix=%q", banner, prefix)
	}
}

func TestFrameWithBannerCursorPosition(t *testing.T) {
	t.Parallel()

	// A banner line above the edit line shifts the cursor row down.
	lines := []string{"╭─ banner", "❯ echo", "> more"}
	_, curRow, curCol := frame(lines, 40, 1, 6)
	if curRow != 1 || curCol != 6 {
		t.Errorf("cursor = (%d,%d), want (1,6)", curRow, curCol)
	}
}
