package editor_test

import (
	"testing"

	"github.com/blairham/koi-shell/internal/editor"
	"github.com/blairham/koi-shell/internal/term"
)

// Round 2 of the readline keymap (#118) and numeric arguments (#116).
//
// Nobody abandons a shell over a missing Alt-u — round 1 took the
// bindings that do have abandonment stories behind them. These are here
// because "nothing missing in the first hour" eventually means the whole
// keymap, and because each one is a leaf function over machinery that
// already exists.

func TestReadlineRoundTwoBindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []term.Event
		want   string
	}{
		{
			"Alt-u upcases the word",
			append(typed("echo hello world"), ctrl('a'), alt('f'), alt('u')),
			"echo HELLO world",
		},
		{
			"Alt-l downcases the word",
			append(typed("echo HELLO"), ctrl('a'), alt('f'), alt('l')),
			"echo hello",
		},
		{
			"Alt-c capitalizes the word",
			append(typed("echo hello"), ctrl('a'), alt('f'), alt('c')),
			"echo Hello",
		},
		{
			// Ctrl-T does characters; this is the one for two arguments
			// in the wrong order.
			"Alt-t transposes words",
			append(typed("echo second first"), alt('t')),
			"echo first second",
		},
		{
			"Ctrl-V inserts a literal control character",
			append(typed("echo"), ctrl('v'), key(term.KeyTab), typed("x")[0]),
			"echo\tx",
		},
		{
			// Ctrl-] jumps to the next occurrence of the key after it.
			"Ctrl-] character search",
			append(append(typed("echo one two"), ctrl('a'), ctrl(']')), typed("t")[0], typed("X")[0]),
			"echo one Xtwo",
		},
		{
			// Alt-Ctrl-] is the same search, backward.
			"Alt-Ctrl-] searches backward",
			append(typed("echo one two"),
				term.KeyEvent{Key: term.KeyRune, Rune: ']', Mod: term.ModCtrl | term.ModAlt},
				typed("o")[0], typed("X")[0]),
			"echo one twXo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := read(t, editor.Config{Prompt: "$ "}, tt.events, keys(key(term.KeyEnter)))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// A numeric argument prefixes any command: this is the structural half
// of #116, and the reason most commands need no code for it at all.
func TestNumericArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []term.Event
		want   string
	}{
		{
			"Alt-4 Ctrl-D deletes four characters",
			append(typed("echo hello"), ctrl('a'), alt('4'), ctrl('d')),
			" hello",
		},
		{
			"Alt-3 Alt-d kills three words",
			append(typed("one two three four"), ctrl('a'), alt('3'), alt('d')),
			" four",
		},
		{
			"a count repeats a typed character",
			append(typed("echo "), alt('8'), typed("-")[0]),
			"echo --------",
		},
		{
			"multi-digit counts accumulate",
			append(typed("x"), alt('1'), alt('2'), typed("y")[0]),
			"xyyyyyyyyyyyy",
		},
		{
			// The count applies to one command and then it is gone.
			"the argument does not leak into the next command",
			append(typed("ab"), ctrl('a'), alt('2'), key(term.KeyRight), typed("!")[0]),
			"ab!",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := read(t, editor.Config{Prompt: "$ "}, tt.events, keys(key(term.KeyEnter)))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// Alt-< walks to the oldest entry and Alt-> comes back to the line being
// typed — which must be the *pending* line, not the last entry visited.
func TestHistoryExtremes(t *testing.T) {
	t.Parallel()

	hist := &fakeHistory{commands: []string{"newest", "middle", "oldest"}}
	got, err := read(t, editor.Config{Prompt: "$ ", History: hist},
		keys(alt('<')), keys(key(term.KeyEnter)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "oldest" {
		t.Errorf("Alt-< = %q, want the oldest entry", got)
	}

	got, err = read(t, editor.Config{Prompt: "$ ", History: hist},
		typed("typing"), keys(alt('<'), alt('>')), keys(key(term.KeyEnter)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "typing" {
		t.Errorf("Alt-> = %q, want the line that was being typed", got)
	}
}

// Alt-r throws away every edit to a recalled line — distinct from undo,
// which steps back one change at a time.
func TestRevertLine(t *testing.T) {
	t.Parallel()

	hist := &fakeHistory{commands: []string{"make build"}}
	got, err := read(t, editor.Config{Prompt: "$ ", History: hist},
		keys(key(term.KeyUp)),
		typed(" --with --several --edits"),
		keys(alt('r'), key(term.KeyEnter)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "make build" {
		t.Errorf("Alt-r = %q, want the recalled line unedited", got)
	}
}

// Ctrl-S is bound at all because raw mode clears IXON: flow control is
// not eating the key, which is the only reason anyone believes the
// binding is lost.
func TestForwardSearch(t *testing.T) {
	t.Parallel()

	hist := &fakeHistory{commands: []string{"make test", "make build", "make lint"}}
	// Reverse-search to the oldest `make`, then walk forward again.
	got, err := read(t, editor.Config{Prompt: "$ ", History: hist},
		keys(ctrl('r')), typed("make"), keys(ctrl('r'), ctrl('r'), ctrl('s'), key(term.KeyEnter)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "make build" {
		t.Errorf("Ctrl-R Ctrl-R Ctrl-S = %q, want the middle match", got)
	}
}
