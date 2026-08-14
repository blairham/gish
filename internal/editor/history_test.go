package editor_test

import (
	"strings"
	"testing"

	"github.com/blairham/gish/internal/editor"
	"github.com/blairham/gish/internal/term"
)

// fakeHistory serves matches from a newest-first command list.
type fakeHistory struct {
	commands []string // newest first, already distinct
}

func (h *fakeHistory) Match(prefix string, n int) (string, bool) {
	return h.nth(n, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func (h *fakeHistory) Search(query string, n int) (string, bool) {
	return h.nth(n, func(c string) bool { return strings.Contains(c, query) })
}

func (h *fakeHistory) nth(n int, match func(string) bool) (string, bool) {
	if n < 0 {
		return "", false
	}
	for _, c := range h.commands {
		if !match(c) {
			continue
		}
		if n == 0 {
			return c, true
		}
		n--
	}
	return "", false
}

func histConfig(commands ...string) editor.Config {
	return editor.Config{
		Prompt:     "$ ",
		ContPrompt: "> ",
		History:    &fakeHistory{commands: commands},
	}
}

func TestHistoryUpRecallsAndDownRestoresPending(t *testing.T) {
	t.Parallel()

	got, err := read(t, histConfig("echo new", "echo old"),
		typed("pending"),
		// Two ups: newest then older; two downs: newest then the pending
		// line comes back — no history entry starts with "pending", so
		// clear it first with ctrl-u to make prefix "" match everything.
		[]term.Event{ctrl('u'), key(term.KeyUp), key(term.KeyUp), key(term.KeyUp), key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	// Third up is exhausted: stays on the oldest.
	if got != "echo old" {
		t.Errorf("got %q", got)
	}
}

func TestHistoryPrefixAware(t *testing.T) {
	t.Parallel()

	got, err := read(t, histConfig("make build", "git push", "git status"),
		typed("git"),
		[]term.Event{key(term.KeyUp), key(term.KeyUp), key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	// "make build" must be skipped: prefix captured as "git".
	if got != "git status" {
		t.Errorf("got %q", got)
	}
}

func TestHistoryDownRestoresPendingLine(t *testing.T) {
	t.Parallel()

	got, err := read(t, histConfig("echo old"),
		typed("keep me"),
		[]term.Event{ctrl('u'), key(term.KeyUp), key(term.KeyDown), ctrl('y'), key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	// Up recalled "echo old", down restored the (empty) pending line,
	// then yank brought back the killed text.
	if got != "keep me" {
		t.Errorf("got %q", got)
	}
}

func TestHistoryEditResetsNavigation(t *testing.T) {
	t.Parallel()

	got, err := read(t, histConfig("git push", "ls"),
		[]term.Event{key(term.KeyUp)}, // recall "git push"
		typed("x"),                    // edit ends navigation
		[]term.Event{key(term.KeyUp), key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	// After the edit, up captures prefix "git pushx" — no match, so the
	// buffer is unchanged.
	if got != "git pushx" {
		t.Errorf("got %q", got)
	}
}

func TestSearchAcceptsMatch(t *testing.T) {
	t.Parallel()

	got, err := read(t, histConfig("make lint", "make build", "git status"),
		[]term.Event{ctrl('r')},
		typed("build"),
		[]term.Event{key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "make build" {
		t.Errorf("got %q", got)
	}
}

func TestSearchStepsToOlderMatch(t *testing.T) {
	t.Parallel()

	got, err := read(t, histConfig("make lint", "make build"),
		[]term.Event{ctrl('r')},
		typed("make"),
		[]term.Event{ctrl('r'), key(term.KeyEnter)}) // second ctrl-r: older match
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "make build" {
		t.Errorf("got %q", got)
	}
}

func TestSearchCancelRestoresBuffer(t *testing.T) {
	t.Parallel()

	got, err := read(t, histConfig("make lint"),
		typed("original"),
		[]term.Event{ctrl('r')},
		typed("lint"),
		[]term.Event{ctrl('g'), key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "original" {
		t.Errorf("got %q", got)
	}
}

func TestSearchExitKeepsCandidateForEditing(t *testing.T) {
	t.Parallel()

	got, err := read(t, histConfig("make lint"),
		[]term.Event{ctrl('r')},
		typed("lint"),
		// Left arrow exits search keeping the candidate, then editing
		// continues on it.
		[]term.Event{key(term.KeyLeft)},
		typed("X"),
		[]term.Event{key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "make linXt" {
		t.Errorf("got %q", got)
	}
}

func TestSearchWithoutHistoryIsInert(t *testing.T) {
	t.Parallel()

	got, err := read(t, editor.Config{Prompt: "$ "},
		[]term.Event{ctrl('r')},
		typed("echo ok"),
		[]term.Event{key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "echo ok" {
		t.Errorf("got %q", got)
	}
}
