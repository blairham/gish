package editor_test

import (
	"context"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/editor"
	"github.com/blairham/koi-shell/internal/term"
)

// readOutput is read() plus what actually reached the terminal, which is
// what a transient prompt is about: the line left behind in scrollback.
func readOutput(t *testing.T, cfg editor.Config, events ...[]term.Event) (string, string) {
	t.Helper()
	var all []term.Event
	for _, evs := range events {
		all = append(all, evs...)
	}
	var out strings.Builder
	ed := editor.New(&fakeTerm{events: all}, &out, cfg)
	line, err := ed.ReadCommand(context.Background())
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	return line, out.String()
}

func TestTransientPromptReplacesTheAcceptedPrompt(t *testing.T) {
	t.Parallel()

	line, out := readOutput(t, editor.Config{
		Prompt:    "~/dev/koi  main\n❯ ",
		Transient: "❯ ",
	}, typed("echo hi"), []term.Event{key(term.KeyEnter)})

	if line != "echo hi" {
		t.Fatalf("line = %q", line)
	}
	// The full prompt was on screen while typing, but the render that
	// stays behind must not carry it.
	tail := out[strings.LastIndex(out, "echo hi"):]
	if strings.Contains(tail, "main") {
		t.Errorf("accepted line kept the full prompt: %q", tail)
	}
}

func TestTransientPromptDropsTheRightPrompt(t *testing.T) {
	t.Parallel()

	_, out := readOutput(t, editor.Config{
		Prompt:    "❯ ",
		RPrompt:   "14:05:06",
		Transient: "❯ ",
	}, typed("ls"), []term.Event{key(term.KeyEnter)})

	// A clock frozen at the moment a command ran is worse than no clock:
	// it reads as the time the output appeared.
	tail := out[strings.LastIndex(out, "ls"):]
	if strings.Contains(tail, "14:05:06") {
		t.Errorf("accepted line kept the right prompt: %q", tail)
	}
}

func TestWithoutTransientTheAcceptedPromptStands(t *testing.T) {
	t.Parallel()

	_, out := readOutput(t, editor.Config{Prompt: "~/dev/koi\n❯ "},
		typed("ls"), []term.Event{key(term.KeyEnter)})

	if !strings.Contains(out, "~/dev/koi") {
		t.Errorf("prompt vanished without anyone asking: %q", out)
	}
}
