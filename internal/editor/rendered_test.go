package editor_test

import (
	"strings"
	"testing"

	"github.com/blairham/gish/internal/editor"
	"github.com/blairham/gish/internal/term"
)

// Positive assertions that these surfaces reach the screen (#201).
//
// The existing tests for both prove the *absence* of things — the
// transient tests assert a substring is gone, the Tab test asserts the
// accepted line — so deleting the code that draws either one leaves a
// green suite. That is the shape of blind spot #193 was: behavior
// covered, rendering unasserted.

// newEditor builds an editor over the fake terminal and hands back the
// writer, for tests that must configure it (SetRPrompt and friends)
// between construction and the read.
func newEditor(t *testing.T, cfg editor.Config, events ...[]term.Event) (*editor.Editor, *strings.Builder) {
	t.Helper()
	var all []term.Event
	for _, evs := range events {
		all = append(all, evs...)
	}
	out := &strings.Builder{}
	return editor.New(&fakeTerm{events: all}, out, cfg), out
}

// The right prompt is produced by the theme and set on the editor, but
// no test drove it through a real render at a real width — the one
// existing test calls the pure layout helper with a hand-fed width, and
// the only Editor-level test asserts a right prompt is *not* shown.
func TestRPromptReachesTheScreen(t *testing.T) {
	t.Parallel()

	ed, out := newEditor(t, editor.Config{Prompt: "$ "}, typed("ab"), []term.Event{key(term.KeyEnter)})
	ed.SetRPrompt("\x1b[2m12:00\x1b[0m")
	if _, err := ed.ReadCommand(t.Context()); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "\x1b[2m12:00\x1b[0m") {
		t.Fatalf("right prompt never rendered: %q", got)
	}
	// fakeTerm is 80 wide and "$ ab" is 4, so the right prompt is held
	// one column short of the edge: 80-1-5 = column 74, i.e. 70 spaces
	// of gap. Asserting the gap rather than mere presence is what
	// catches a right prompt that renders butted against the text.
	if !strings.Contains(got, "$ ab"+strings.Repeat(" ", 70)+"\x1b[2m12:00") {
		t.Errorf("right prompt not held at the right edge: %q", got)
	}
}

// A typed line long enough to reach the right prompt hides it, zsh-style
// — it must never wrap or be overwritten by what is typed.
func TestRPromptHidesWhenTheLineReachesIt(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 76) // "$ " + 76 = 78, leaving no room for "12:00"
	ed, out := newEditor(t, editor.Config{Prompt: "$ "}, typed(long), []term.Event{key(term.KeyEnter)})
	ed.SetRPrompt("12:00")
	if _, err := ed.ReadCommand(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The final frame — after the whole line is typed — must not carry it.
	got := out.String()
	final := got[strings.LastIndex(got, long):]
	if strings.Contains(final, "12:00") {
		t.Errorf("right prompt survived a line that reached it: %q", final)
	}
}

// The Tab candidate listing: TestTabListRendersAndClears never inspects
// the writer despite its name, so nothing proved the list is drawn.
func TestCandidateListIsDrawnBelowTheLine(t *testing.T) {
	t.Parallel()

	// Two candidates sharing a prefix: Tab completes the common prefix,
	// a second Tab has no progress to make and lists instead.
	_, out := readOutput(t,
		completeConfig(
			editor.Candidate{Value: "alpha", Display: "alpha"},
			editor.Candidate{Value: "alpine", Display: "alpine"},
		),
		typed("al"),
		[]term.Event{key(term.KeyTab), key(term.KeyTab), key(term.KeyEnter)})

	if !strings.Contains(out, "alpha") || !strings.Contains(out, "alpine") {
		t.Fatalf("candidate list never rendered: %q", out)
	}
	// Both on one row, in one frame, below the edit line — a listing
	// drawn above the line or one-per-line is a different (broken) UI
	// that a substring check alone would accept. The column is the
	// widest candidate plus two, so "alpha" pads by three.
	row := "alpha" + strings.Repeat(" ", 3) + "alpine"
	if !strings.Contains(out, row) {
		t.Errorf("candidates not columnized on one row (%q): %q", row, out)
	}
	if idx := strings.Index(out, row); idx > 0 && !strings.Contains(out[:idx], "alp") {
		t.Error("the listing was drawn before the edit line, not below it")
	}
}
