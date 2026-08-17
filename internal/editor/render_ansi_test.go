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

// TestDisplayWidthIgnoresStringEscapes pins the sequences whose payload
// is arbitrary text rather than numeric parameters.
//
// This is a regression test with a visible symptom: the OSC 133;B mark
// closes every prompt gish renders (#99), and measuring its payload as
// five columns put the cursor five cells right of the prompt character
// on every keystroke. It looked like the theme had a trailing gap.
func TestDisplayWidthIgnoresStringEscapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		s    string
		want int
	}{
		{"OSC 133 prompt end, ST-terminated", "\x1b]133;B\x1b\\", 0},
		{"prompt closed by the mark", "\x1b[38;5;76m❯\x1b[0m \x1b]133;B\x1b\\", 2},
		{"OSC 133 prompt start leads", "\x1b]133;A\x1b\\~/x", 3},
		{"OSC 0 title, BEL-terminated", "\x1b]0;a title\ax", 1},
		{"DCS passthrough", "\x1bPtmux;\x1b\x1b]52;c;QQ==\a\x1b\\y", 1},
		{"unterminated OSC swallows the rest", "\x1b]133;B", 0},
		{"lone ST is still two bytes", "\x1b\\z", 1},
	}
	for _, tt := range tests {
		if got := displayWidth(tt.s); got != tt.want {
			t.Errorf("%s: displayWidth(%q) = %d, want %d", tt.name, tt.s, got, tt.want)
		}
	}
}

// TestWrapLineKeepsStringEscapesAtomic: a mark split across rows would
// both mis-measure and corrupt the sequence the terminal parses.
func TestWrapLineKeepsStringEscapesAtomic(t *testing.T) {
	t.Parallel()

	rows := wrapLine("ab\x1b]133;B\x1b\\cde", 3)
	if len(rows) != 2 {
		t.Fatalf("rows = %q, want 2", rows)
	}
	if !strings.Contains(rows[0], "\x1b]133;B\x1b\\") {
		t.Errorf("mark split across rows: %q", rows)
	}
	if got, want := displayWidth(rows[0]), 3; got != want {
		t.Errorf("row 0 width = %d, want %d", got, want)
	}
	if got, want := displayWidth(rows[1]), 2; got != want {
		t.Errorf("row 1 width = %d, want %d", got, want)
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
