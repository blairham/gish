package editor_test

import (
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/editor"
	"github.com/blairham/koi-shell/internal/term"
)

func completeConfig(cands ...editor.Candidate) editor.Config {
	return editor.Config{
		Prompt: "$ ",
		Complete: func(text string, cursor int) editor.CompleteResult {
			// Complete the last word.
			start := strings.LastIndexByte(text[:cursor], ' ') + 1
			word := text[start:cursor]
			var matched []editor.Candidate
			for _, c := range cands {
				if strings.HasPrefix(c.Value, word) {
					matched = append(matched, c)
				}
			}
			return editor.CompleteResult{WordStart: start, Candidates: matched}
		},
	}
}

func TestTabCompletesUniqueWithSpace(t *testing.T) {
	t.Parallel()

	got, err := read(t, completeConfig(editor.Candidate{Value: "restart", Display: "restart"}),
		typed("res"), []term.Event{key(term.KeyTab)}, typed("now"), []term.Event{key(term.KeyEnter)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "restart now" {
		t.Errorf("got %q", got)
	}
}

func TestTabCompletesDirectoryWithoutSpace(t *testing.T) {
	t.Parallel()

	got, err := read(t, completeConfig(editor.Candidate{Value: "src/", Display: "src/"}),
		typed("s"), []term.Event{key(term.KeyTab)}, typed("x"), []term.Event{key(term.KeyEnter)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "src/x" {
		t.Errorf("got %q", got)
	}
}

func TestTabInsertsCommonPrefix(t *testing.T) {
	t.Parallel()

	cfg := completeConfig(
		editor.Candidate{Value: "release.txt", Display: "release.txt"},
		editor.Candidate{Value: "readme.md", Display: "readme.md"},
	)
	got, err := read(t, cfg,
		typed("cat r"), []term.Event{key(term.KeyTab), key(term.KeyEnter)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "cat re" {
		t.Errorf("got %q", got)
	}
}

func TestTabEscapesInsertedValue(t *testing.T) {
	t.Parallel()

	got, err := read(t, completeConfig(editor.Candidate{Value: "my file.txt", Display: "my file.txt"}),
		typed("cat m"), []term.Event{key(term.KeyTab), key(term.KeyEnter)})
	if err != nil {
		t.Fatal(err)
	}
	if got != `cat my\ file.txt ` {
		t.Errorf("got %q", got)
	}
}

func TestTabWithoutCompleterIsInert(t *testing.T) {
	t.Parallel()

	got, err := read(t, editor.Config{Prompt: "$ "},
		typed("abc"), []term.Event{key(term.KeyTab), key(term.KeyEnter)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc" {
		t.Errorf("got %q", got)
	}
}

func TestTabListRendersAndClears(t *testing.T) {
	t.Parallel()

	cfg := completeConfig(
		editor.Candidate{Value: "alpha", Display: "alpha"},
		editor.Candidate{Value: "alpine", Display: "alpine"},
	)
	// Two tabs: first inserts "alp" (common prefix), second shows the
	// list; typing continues normally afterwards.
	got, err := read(t, cfg,
		typed("a"),
		[]term.Event{key(term.KeyTab), key(term.KeyTab)},
		typed("ha"),
		[]term.Event{key(term.KeyEnter)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "alpha" {
		t.Errorf("got %q", got)
	}
}
