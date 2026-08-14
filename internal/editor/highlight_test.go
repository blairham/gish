package editor

import (
	"strings"
	"testing"
)

func TestApplyHighlight(t *testing.T) {
	t.Parallel()

	lines := applyHighlight([]string{"gti status"}, []HighlightSpan{
		{Start: 0, End: 3, Style: "\x1b[31m"},
	})
	want := "\x1b[31mgti\x1b[0m status"
	if lines[0] != want {
		t.Errorf("line = %q, want %q", lines[0], want)
	}
}

func TestApplyHighlightMultiline(t *testing.T) {
	t.Parallel()

	// Buffer "for x\ndone": span on "done" (runes 6..10 of the whole
	// text) must land on line 1 only.
	lines := applyHighlight([]string{"for x", "done"}, []HighlightSpan{
		{Start: 6, End: 10, Style: "\x1b[1m"},
	})
	if lines[0] != "for x" {
		t.Errorf("line0 = %q", lines[0])
	}
	if lines[1] != "\x1b[1mdone\x1b[0m" {
		t.Errorf("line1 = %q", lines[1])
	}
}

func TestApplyHighlightOverlapSkipsRemainder(t *testing.T) {
	t.Parallel()

	lines := applyHighlight([]string{"abcdef"}, []HighlightSpan{
		{Start: 0, End: 4, Style: "\x1b[33m"},
		{Start: 2, End: 6, Style: "\x1b[36m"}, // overlaps: dropped
	})
	if strings.Count(lines[0], "\x1b[33m") != 1 || strings.Contains(lines[0], "\x1b[36m") {
		t.Errorf("line = %q", lines[0])
	}
}
