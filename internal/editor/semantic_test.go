package editor_test

import (
	"strings"
	"testing"

	"github.com/blairham/gish/internal/editor"
	"github.com/blairham/gish/internal/term"
)

// The OSC 133 semantic marks as internal/repl wraps a prompt in them
// (#99). Spelled out here rather than imported because this is the
// contract *between* the two packages: repl promises the marks are
// zero-width, and the editor is where that promise is kept or broken.
const (
	oscPromptStart = "\x1b]133;A\x1b\\"
	oscPromptEnd   = "\x1b]133;B\x1b\\"
)

// TestSemanticMarksCostNothingOnScreen is the regression test for the
// gap after the prompt character.
//
// The marks close every prompt gish renders, and the renderer measured
// the "133;B" payload as five printable columns — so the cursor sat five
// cells right of the prompt, on every keystroke, in every theme. It read
// as a theme with a trailing gap, which is why it went unnoticed: the
// prompt looked plausible.
//
// The invariant is stated as a byte comparison rather than a width
// assertion, because it is the whole claim: adding the marks may add
// their own bytes to the stream and must change nothing else — not
// cursor movement, not wrapping, not what is repainted.
func TestSemanticMarksCostNothingOnScreen(t *testing.T) {
	t.Parallel()

	keys := [][]term.Event{typed("echo hi"), {key(term.KeyEnter)}}

	_, bare := readOutput(t, editor.Config{Prompt: "❯ "}, keys...)
	_, marked := readOutput(t, editor.Config{
		Prompt: oscPromptStart + "❯ " + oscPromptEnd,
	}, keys...)

	if !strings.Contains(marked, oscPromptEnd) {
		t.Fatalf("the marks never reached the terminal: %q", marked)
	}
	stripped := strings.ReplaceAll(marked, oscPromptStart, "")
	stripped = strings.ReplaceAll(stripped, oscPromptEnd, "")
	if stripped != bare {
		t.Errorf("marks changed the render beyond their own bytes:\n with marks (stripped): %q\n without marks:          %q", stripped, bare)
	}
}

// TestSemanticMarksSurviveAWrappedPrompt covers the failure the width bug
// hid behind: a mark whose payload counts as printable is also a mark the
// wrapper is willing to break across rows. A split OSC 133 sequence is
// not a cosmetic problem — the terminal parses garbage and silently stops
// offering block navigation, which is the entire point of emitting it.
func TestSemanticMarksSurviveAWrappedPrompt(t *testing.T) {
	t.Parallel()

	// A prompt long enough to wrap the fake terminal's width.
	long := strings.Repeat("ab/", 40)
	_, out := readOutput(t, editor.Config{
		Prompt: oscPromptStart + long + "❯ " + oscPromptEnd,
	}, typed("x"), []term.Event{key(term.KeyEnter)})

	for _, mark := range []string{oscPromptStart, oscPromptEnd} {
		if !strings.Contains(out, mark) {
			t.Errorf("mark %q did not survive wrapping intact: %q", mark, out)
		}
	}
}
