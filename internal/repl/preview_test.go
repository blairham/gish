package repl

import (
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/history"
)

// A command that greets before it works would preview its least useful
// line. What someone scanning ctrl-r wants is the line that tells them
// what happened — usually the failure.
func TestPreviewPrefersTheErrorLine(t *testing.T) {
	out := "Cloning into 'repo'...\nremote: Counting objects\nfatal: repository not found\n"
	got := firstInterestingLine(out)
	if !strings.Contains(got, "fatal: repository not found") {
		t.Errorf("preview = %q, want the fatal line", got)
	}
}

// With nothing that looks like a failure, the first real line stands in.
func TestPreviewFallsBackToTheFirstLine(t *testing.T) {
	out := "\n\n   \nbuilt 3 targets\nall good\n"
	if got := firstInterestingLine(out); got != "built 3 targets" {
		t.Errorf("preview = %q", got)
	}
}

// Captured output is full of color, and the picker renders the preview
// dim — a stray color code would fight that.
func TestPreviewStripsColour(t *testing.T) {
	out := "\x1b[31merror:\x1b[0m missing symbol\n"
	got := firstInterestingLine(out)
	if strings.Contains(got, "\x1b") {
		t.Errorf("escape survived: %q", got)
	}
	if !strings.Contains(got, "error: missing symbol") {
		t.Errorf("preview = %q", got)
	}
}

// TestPreviewStripsHyperlinks: OSC 8 is the OSC most likely to be in a
// captured build log, and the tools that emit it close with ST rather
// than BEL. Accepting only BEL left "8;;<url>" in the preview and, worse,
// let the match run on to a BEL arriving later in the log — eating the
// error the preview exists to show.
func TestPreviewStripsHyperlinks(t *testing.T) {
	out := "\x1b]8;;https://example.com/E0432\x1b\\error[E0432]\x1b]8;;\x1b\\: unresolved import\n"
	got := firstInterestingLine(out)
	if strings.Contains(got, "\x1b") || strings.Contains(got, "8;;") {
		t.Errorf("hyperlink escape survived: %q", got)
	}
	if got != "error[E0432]: unresolved import" {
		t.Errorf("preview = %q", got)
	}
}

// An unterminated OSC must not swallow the rest of the log.
func TestPreviewSurvivesUnterminatedOSC(t *testing.T) {
	got := firstInterestingLine("\x1b]8;;broken\nfatal: real problem here\n")
	if !strings.Contains(got, "fatal") {
		t.Errorf("unterminated OSC consumed the useful line: %q", got)
	}
}

func TestPreviewOfEmptyOutput(t *testing.T) {
	if got := firstInterestingLine(""); got != "" {
		t.Errorf("empty output previewed %q", got)
	}
	if got := firstInterestingLine("\n\n\n"); got != "" {
		t.Errorf("blank output previewed %q", got)
	}
}

// No block store, no previews — and crucially no panic. Capture is
// opt-in, so this is the common case.
func TestPreviewsWithoutAStore(t *testing.T) {
	saved := blockStore
	blockStore = nil
	defer func() { blockStore = saved }()

	entries := []history.Entry{{Command: "ls", Block: "someref"}, {Command: "pwd"}}
	got := outputPreviews(entries)
	if len(got) != len(entries) {
		t.Fatalf("previews len = %d, want %d", len(got), len(entries))
	}
	for i, p := range got {
		if p != "" {
			t.Errorf("entry %d previewed %q with no store", i, p)
		}
	}
}

// An entry with no block is the normal case even with capture on:
// stderr and builtin output are not captured, so a missing preview must
// never read as breakage.
func TestPreviewsSkipEntriesWithoutBlocks(t *testing.T) {
	entries := make([]history.Entry, 3)
	for i := range entries {
		entries[i] = history.Entry{Command: "echo hi"}
	}
	for i, p := range outputPreviews(entries) {
		if p != "" {
			t.Errorf("entry %d previewed %q without a block", i, p)
		}
	}
}

// The scan is bounded so ctrl-r stays instant: the picker builds every
// row before it paints, and a file read per row across the whole
// history would be paid on every keystroke that opens it.
func TestPreviewsAreBounded(t *testing.T) {
	entries := make([]history.Entry, previewCount+50)
	for i := range entries {
		entries[i] = history.Entry{Command: "cmd", Block: "nonexistent"}
	}
	got := outputPreviews(entries)
	if len(got) != len(entries) {
		t.Fatalf("previews len = %d, want one per entry", len(got))
	}
	for i := previewCount; i < len(got); i++ {
		if got[i] != "" {
			t.Errorf("entry %d past the bound was previewed", i)
		}
	}
}

func TestLooksLikeError(t *testing.T) {
	for _, yes := range []string{
		"error: nope", "FATAL: bad", "build failed", "panic: runtime",
		"cannot open file", "no such file or directory",
	} {
		if !looksLikeError(yes) {
			t.Errorf("%q not recognized as an error", yes)
		}
	}
	for _, no := range []string{"built 3 targets", "all tests passed", "cloning into repo"} {
		if looksLikeError(no) {
			t.Errorf("%q wrongly recognized as an error", no)
		}
	}
}
