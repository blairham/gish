package editor_test

import (
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/editor"
	"github.com/blairham/koi-shell/internal/term"
)

// The vi-mode suite (#163).
//
// The cases are the ones people name when they explain why they went
// back to zsh — `ciw`, `d2w`, counts, text objects — rather than the
// ones that are easy to implement. A vi mode that handles `x` and `dd`
// and nothing else is what "the vim emulator sucks" describes.

func esc() term.Event { return term.KeyEvent{Key: term.KeyEscape} }

// viRead types keys through a vi-mode editor and returns the accepted
// line. The first argument is typed in insert mode; Escape is explicit,
// so each case reads like the keystrokes a user would actually make.
func viRead(t *testing.T, events ...[]term.Event) string {
	t.Helper()
	got, err := read(t, editor.Config{Prompt: "$ ", EditMode: editor.ModeVi}, events...)
	if err != nil {
		t.Fatalf("ReadCommand: %v", err)
	}
	return got
}

func keys(evs ...term.Event) []term.Event { return evs }

func TestViModeStartsInInsert(t *testing.T) {
	t.Parallel()

	// No mode switch: a vi user's line still types like a line.
	if got := viRead(t, typed("echo hi"), keys(key(term.KeyEnter))); got != "echo hi" {
		t.Errorf("got %q", got)
	}
}

func TestViMotionsAndEdits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		typed string
		cmd   string // normal-mode keys after Escape
		want  string
	}{
		// The composition cases: an operator against a counted motion.
		{"dw", "echo one two three", "0wdw", "echo two three"},
		{"d2w", "echo one two three", "0wd2w", "echo three"},
		{"2dw", "echo one two three", "02dw", "two three"},
		{"cw", "echo one", "0cwprintf", "printf one"},
		{"db", "echo one two", "0wwdbimid ", "echo mid two"},
		{"de", "echo one two", "0wde", "echo  two"},

		// Text objects — the case that gets named by itself.
		{"ciw on a word", "echo middle end", "0wciwNEW", "echo NEW end"},
		{"diw", "echo middle end", "0wdiw", "echo  end"},
		{"daw", "echo middle end", "0wdaw", "echo end"},
		{"ci-quote", `echo "some text" tail`, `0wci"NEW`, `echo "NEW" tail`},
		{"ca-quote", `echo "some text" tail`, `0wca"X`, "echo X tail"},
		{"ci-paren", "echo (a b) c", "0wci(Z", "echo (Z) c"},
		{"di-brace", "echo {a b} c", "0wdi{", "echo {} c"},

		// Single-key edits and counts.
		{"x", "echoo hi", "0xx", "hoo hi"},
		{"3x", "echo hi", "03x", "o hi"},
		{"D", "echo one two", "0wD", "echo "},
		{"C", "echo one", "0wCtwo", "echo two"},
		{"r", "echo hi", "0rE", "Echo hi"},
		{"3r", "echo hi", "03rx", "xxxo hi"},
		{"~", "echo hi", "0~~", "ECho hi"},
		{"s", "echo hi", "0sX", "Xcho hi"},
		{"S", "echo hi", "0Snew line", "new line"},

		// Motions that place the cursor for an insert.
		{"A", "echo", "Ao hi", "echoo hi"},
		{"I", "echo hi", "Isudo ", "sudo echo hi"},
		{"a", "echo", "0a!", "e!cho"},
		{"$", "ab cd", "0$icut", "ab ccutd"},
		// An unbound key in normal mode types nothing: mistiming Escape
		// must not scatter letters through the command line.
		{"unbound keys are no-ops", "echo hi", "0zQiX", "Xecho hi"},
		{"^", "  indented", "0^iX", "  Xindented"},
		{"f", "echo one two", "0fnix", "echo oxne two"},
		{"t", "echo one two", "0tnix", "echo xone two"},
		{"F", "echo one two", "$Feix", "echo onxe two"},
		{"df-inclusive", "echo one two", "0dfo", " one two"},
		{"dt", "echo one two", "0dto", "o one two"},
		{"semicolon repeats find", "a.b.c.d", "0f.;ix", "a.bx.c.d"},

		// Whole-line operators.
		{"dd", "only line", "dd", ""},
		{"cc", "echo old", "ccecho new", "echo new"},
		{"yy then p", "dup", "yyp", "dup\ndup"},
		{"p pastes after", "abc", "0xp", "bac"},
		{"P pastes before", "abc", "0x$P", "bac"},

		// u undoes the last change, which in vi mode is the whole
		// operator rather than one keystroke.
		{"u", "echo hi", "0dwu", "echo hi"},

		// Big-word variants: WORDs ignore punctuation, words do not.
		{"dW", "echo a-b-c end", "0wdW", "echo end"},
		{"dw stops at punctuation", "echo a-b end", "0wdw", "echo -b end"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := viRead(t,
				typed(tt.typed),
				keys(esc()),
				typed(tt.cmd),
				keys(key(term.KeyEnter)))
			if got != tt.want {
				t.Errorf("typed %q then %q\n got %q\nwant %q", tt.typed, tt.cmd, got, tt.want)
			}
		})
	}
}

// Escape must leave the cursor *on* the last character, not past it —
// this is what makes `A` and `a` different, and getting it wrong makes
// every append-at-end land one character early.
func TestViEscapeMovesCursorLeft(t *testing.T) {
	t.Parallel()

	// `i` inserts before the character the cursor is on. After Escape
	// that character is the last one, so a step-back that did not happen
	// shows up here as an insert past the end.
	if got := viRead(t, typed("echo"), keys(esc()), typed("iX"), keys(key(term.KeyEnter))); got != "echXo" {
		t.Errorf("insert after Escape = %q, want %q (cursor did not step back)", got, "echXo")
	}
	// `a` appends after it, which is the difference the step-back buys.
	if got := viRead(t, typed("echo"), keys(esc()), typed("aX"), keys(key(term.KeyEnter))); got != "echoX" {
		t.Errorf("append after Escape = %q, want %q", got, "echoX")
	}
}

// Insert mode keeps the emacs keymap. A shell is not vi: hitting Escape
// to reach the start of a line you are still typing is a papercut of its
// own, and Ctrl-A costing a mode switch would be a worse trade than the
// purity is worth.
func TestViInsertModeKeepsEmacsKeys(t *testing.T) {
	t.Parallel()

	got := viRead(t, typed("echo hi"), keys(ctrl('a')), typed("sudo "), keys(key(term.KeyEnter)))
	if got != "sudo echo hi" {
		t.Errorf("got %q", got)
	}
}

// Control chords work in normal mode too, without being bound twice:
// normal mode declines them and the one keymap answers.
func TestViNormalModePassesControlChords(t *testing.T) {
	t.Parallel()

	_, err := read(t, editor.Config{Prompt: "$ ", EditMode: editor.ModeVi},
		typed("doomed"), keys(esc(), ctrl('c')))
	if err == nil {
		t.Fatal("Ctrl-C in normal mode did not interrupt")
	}
}

// k and j walk history on a one-line buffer (bash's binding) and move by
// line on a multi-line one — where "up" plainly means the line above,
// and preferring history would make the buffer uneditable by exactly the
// person who reached for vi mode to edit it.
func TestViHistoryVersusLineMovement(t *testing.T) {
	t.Parallel()

	hist := &fakeHistory{commands: []string{"second", "first"}}
	got, err := read(t, editor.Config{Prompt: "$ ", EditMode: editor.ModeVi, History: hist},
		keys(esc()), typed("k"), keys(key(term.KeyEnter)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "second" {
		t.Errorf("k on an empty line = %q, want the previous history entry", got)
	}

	// Multi-line: k moves up a line, and an insert lands on that line.
	got = viRead(t,
		typed("one"),
		keys(term.KeyEvent{Key: term.KeyEnter, Mod: term.ModAlt}),
		typed("two"),
		keys(esc()),
		typed("kIX"),
		keys(key(term.KeyEnter)))
	if got != "Xone\ntwo" {
		t.Errorf("k in a multi-line buffer = %q, want %q", got, "Xone\ntwo")
	}
}

// The mode is visible: a vi user reads it off the cursor shape before
// anything else, and a terminal that does not implement DECSCUSR
// ignores a zero-width sequence.
func TestViModeReportsCursorShape(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	ed := editor.New(&fakeTerm{events: append(append(typed("echo"), esc()), key(term.KeyEnter))},
		&out, editor.Config{Prompt: "$ ", EditMode: editor.ModeVi})
	if _, err := ed.ReadCommand(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\x1b[2 q") {
		t.Error("normal mode did not ask for a block cursor")
	}
	if !strings.Contains(out.String(), "\x1b[6 q") {
		t.Error("insert mode did not ask for a bar cursor")
	}
}

// Emacs mode is untouched: Escape is not a mode switch there, and the
// default must stay the default.
func TestEmacsModeIgnoresEscape(t *testing.T) {
	t.Parallel()

	got, err := read(t, editor.Config{Prompt: "$ "},
		typed("echo"), keys(esc()), typed("x"), keys(key(term.KeyEnter)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "echox" {
		t.Errorf("got %q, want %q — Escape must not switch modes in emacs mode", got, "echox")
	}
}

// Escape and Alt are the same byte on the wire, and a terminal hands a
// vi user's `<Esc>w` over as one chunk more often than not. In vi mode
// the ambiguity resolves toward Escape, or the mode is unusable at
// typing speed — which is exactly how it failed before this rule.
func TestViTreatsAltAsEscape(t *testing.T) {
	t.Parallel()

	got := viRead(t,
		typed("echo WRONG tail"),
		keys(alt('b')), // <Esc>b, arriving glued together
		typed("bciwRIGHT"),
		keys(key(term.KeyEnter)))
	if got != "echo RIGHT tail" {
		t.Errorf("got %q, want %q", got, "echo RIGHT tail")
	}
}
