package editor_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/blairham/koi-shell/internal/editor"
	"github.com/blairham/koi-shell/internal/term"
)

// fakeTerm scripts a sequence of input events. Events are *consumed*:
// a second Events call (the editor restarts decoding around an
// external edit) picks up where the first left off, like a real
// terminal — replaying would loop forever.
type fakeTerm struct {
	mu     sync.Mutex
	events []term.Event
	pos    int
}

func (f *fakeTerm) EnterRaw() (func() error, error) { return func() error { return nil }, nil }
func (f *fakeTerm) Size() (int, int, error)         { return 80, 24, nil }

func (f *fakeTerm) Events(ctx context.Context) (<-chan term.Event, error) {
	ch := make(chan term.Event)
	go func() {
		defer close(ch)
		for {
			// The lock is held across the send, which is what makes
			// "advance only on delivery" actually atomic.
			//
			// Reading the position, unlocking to send, then locking again
			// to advance leaves a window: the handover cancels this
			// context the moment it takes an event, and the next decoder
			// session could read the same position and redeliver it. One
			// duplicated keystroke shifts the whole script by one, so the
			// final Enter never arrives and the read ends in EOF —
			// TestExternalEditReplacesBuffer failing as `"", EOF`, on CI
			// and about once in forty runs locally.
			//
			// Holding the lock is safe because only one session is live at
			// a time: the editor cancels this context before opening the
			// next, which releases a send that nobody is reading.
			f.mu.Lock()
			if f.pos >= len(f.events) {
				f.mu.Unlock()
				return
			}
			ev := f.events[f.pos]
			select {
			case ch <- ev:
				// Advance only on delivery: an event still in flight
				// when the context dies must survive for the next
				// decoder session (the shell's type-ahead rule).
				f.pos++
				f.mu.Unlock()
			case <-ctx.Done():
				f.mu.Unlock()
				return
			}
		}
	}()
	return ch, nil
}

func typed(s string) []term.Event {
	evs := make([]term.Event, 0, len(s))
	for _, r := range s {
		evs = append(evs, term.KeyEvent{Key: term.KeyRune, Rune: r})
	}
	return evs
}

func ctrl(r rune) term.Event { return term.KeyEvent{Key: term.KeyRune, Rune: r, Mod: term.ModCtrl} }

func alt(r rune) term.Event     { return term.KeyEvent{Key: term.KeyRune, Rune: r, Mod: term.ModAlt} }
func key(k term.Key) term.Event { return term.KeyEvent{Key: k} }

func read(t *testing.T, cfg editor.Config, events ...[]term.Event) (string, error) {
	t.Helper()
	var all []term.Event
	for _, evs := range events {
		all = append(all, evs...)
	}
	var out strings.Builder
	ed := editor.New(&fakeTerm{events: all}, &out, cfg)
	return ed.ReadCommand(context.Background())
}

func TestReadCommandTypesAndAccepts(t *testing.T) {
	t.Parallel()

	got, err := read(t, editor.Config{Prompt: "$ "},
		typed("echo hi"), []term.Event{key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "echo hi" {
		t.Errorf("got %q", got)
	}
}

func TestReadCommandEditing(t *testing.T) {
	t.Parallel()

	// Type a wrong prefix, jump home, delete it.
	got, err := read(t, editor.Config{Prompt: "$ "},
		typed("xecho hi"),
		[]term.Event{ctrl('a'), key(term.KeyDelete), key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "echo hi" {
		t.Errorf("got %q", got)
	}
}

func TestReadCommandInterrupt(t *testing.T) {
	t.Parallel()

	_, err := read(t, editor.Config{Prompt: "$ "},
		typed("doomed"), []term.Event{ctrl('c')})
	if !errors.Is(err, editor.ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
}

func TestReadCommandEOFOnEmpty(t *testing.T) {
	t.Parallel()

	_, err := read(t, editor.Config{Prompt: "$ "}, []term.Event{ctrl('d')})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestReadCommandCtrlDDeletesWhenNotEmpty(t *testing.T) {
	t.Parallel()

	got, err := read(t, editor.Config{Prompt: "$ "},
		typed("ab"),
		[]term.Event{ctrl('a'), ctrl('d'), key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "b" {
		t.Errorf("got %q", got)
	}
}

func TestReadCommandMultilineContinuation(t *testing.T) {
	t.Parallel()

	cfg := editor.Config{
		Prompt:     "$ ",
		ContPrompt: "> ",
		AcceptWhen: func(s string) bool { return strings.Contains(s, "done") },
	}
	got, err := read(t, cfg,
		typed("for x; do"), []term.Event{key(term.KeyEnter)}, // incomplete: newline
		typed("done"), []term.Event{key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "for x; do\ndone" {
		t.Errorf("got %q", got)
	}
}

func TestReadCommandKillYank(t *testing.T) {
	t.Parallel()

	// Kill two words backward (coalesced), then yank them back.
	got, err := read(t, editor.Config{Prompt: "$ "},
		typed("git commit now"),
		[]term.Event{ctrl('w'), ctrl('w'), ctrl('y'), key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "git commit now" {
		t.Errorf("got %q", got)
	}
}

func TestReadCommandYankPop(t *testing.T) {
	t.Parallel()

	// Kill "two", then "one" (separate kills), yank "one", pop to "two".
	got, err := read(t, editor.Config{Prompt: "$ "},
		typed("two"),
		[]term.Event{ctrl('w')},
		typed("one"),
		// Move between kills so they don't coalesce: the left arrow
		// breaks the kill run.
		[]term.Event{key(term.KeyLeft), key(term.KeyRight), ctrl('w'), ctrl('y'), alt('y'), key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "two" {
		t.Errorf("got %q", got)
	}
}

func TestReadCommandUndo(t *testing.T) {
	t.Parallel()

	got, err := read(t, editor.Config{Prompt: "$ "},
		typed("echo keep"),
		[]term.Event{ctrl('u'), ctrl('_'), key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "echo keep" {
		t.Errorf("got %q", got)
	}
}

func TestReadCommandPaste(t *testing.T) {
	t.Parallel()

	got, err := read(t, editor.Config{Prompt: "$ "},
		[]term.Event{term.PasteEvent{Text: "ls -la\r\ncd /tmp"}, key(term.KeyEnter)})
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	if got != "ls -la\ncd /tmp" {
		t.Errorf("got %q", got)
	}
}

func TestReadCommandEventsEndWithoutAccept(t *testing.T) {
	t.Parallel()

	_, err := read(t, editor.Config{Prompt: "$ "}, typed("partial"))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}
