package editor_test

import (
	"context"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/editor"
	"github.com/blairham/koi-shell/internal/term"
)

func suggestConfig(history ...string) editor.Config {
	return editor.Config{
		Prompt: "$ ",
		Suggest: func(text string) string {
			for _, h := range history {
				if strings.HasPrefix(h, text) {
					return h
				}
			}
			return ""
		},
	}
}

func TestGhostAcceptWithRight(t *testing.T) {
	t.Parallel()

	got, err := read(t, suggestConfig("make lint && make test"),
		typed("make li"),
		[]term.Event{key(term.KeyRight), key(term.KeyEnter)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "make lint && make test" {
		t.Errorf("got %q", got)
	}
}

func TestGhostAcceptWithEnd(t *testing.T) {
	t.Parallel()

	got, err := read(t, suggestConfig("git push origin main"),
		typed("git pu"),
		[]term.Event{key(term.KeyEnd), key(term.KeyEnter)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "git push origin main" {
		t.Errorf("got %q", got)
	}
}

func TestRightMovesWhenNotAtEnd(t *testing.T) {
	t.Parallel()

	// Cursor mid-buffer: Right must move, not accept.
	got, err := read(t, suggestConfig("abcdef"),
		typed("abcd"),
		[]term.Event{key(term.KeyLeft), key(term.KeyLeft), key(term.KeyRight)},
		typed("X"),
		[]term.Event{key(term.KeyEnter)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "abcXd" {
		t.Errorf("got %q", got)
	}
}

func TestNoGhostWithoutPrefixMatch(t *testing.T) {
	t.Parallel()

	// Suggestion returns nothing: Right at the end is a no-op move.
	got, err := read(t, suggestConfig("zzz"),
		typed("make"),
		[]term.Event{key(term.KeyRight), key(term.KeyEnter)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "make" {
		t.Errorf("got %q", got)
	}
}

func TestGhostRendersDimmed(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	events := append(typed("make li"), key(term.KeyEnter))
	ed := editor.New(&fakeTerm{events: events}, &out, suggestConfig("make lint"))
	if _, err := ed.ReadCommand(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\x1b[2mnt\x1b[0m") {
		t.Errorf("ghost text not rendered dimmed: %q", out.String())
	}
}
