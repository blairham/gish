package editor_test

import (
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/editor"
	"github.com/blairham/koi-shell/internal/term"
)

// muscleCfg is a config with history, for the parity commands.
func muscleCfg(commands ...string) editor.Config {
	return editor.Config{
		Prompt:  "koi$ ",
		History: &fakeHistory{commands: commands},
	}
}

func TestYankLastArgCyclesOlderEntries(t *testing.T) {
	t.Parallel()

	cfg := muscleCfg("git commit -m draft ./notes.md", "make test ./pkg", "cd /srv")
	// One Alt-. takes the newest entry's last word.
	got, err := read(t, cfg, typed("vim "), []term.Event{alt('.')}, []term.Event{key(term.KeyEnter)})
	if err != nil || got != "vim ./notes.md" {
		t.Fatalf("single Alt-. = %q, %v", got, err)
	}
	// Repeating replaces it with progressively older last-args.
	got, err = read(t, cfg, typed("vim "),
		[]term.Event{alt('.'), alt('.')}, []term.Event{key(term.KeyEnter)})
	if err != nil || got != "vim ./pkg" {
		t.Fatalf("double Alt-. = %q, %v", got, err)
	}
	got, err = read(t, cfg, typed("vim "),
		[]term.Event{alt('.'), alt('.'), alt('.')}, []term.Event{key(term.KeyEnter)})
	if err != nil || got != "vim /srv" {
		t.Fatalf("triple Alt-. = %q, %v", got, err)
	}
	// Alt-_ is the same command.
	got, err = read(t, cfg, typed("vim "), []term.Event{alt('_')}, []term.Event{key(term.KeyEnter)})
	if err != nil || got != "vim ./notes.md" {
		t.Fatalf("Alt-_ = %q, %v", got, err)
	}
}

func TestYankLastArgWrapsAndSurvivesEmptyHistory(t *testing.T) {
	t.Parallel()

	// Two entries, three presses: readline wraps to the newest.
	cfg := muscleCfg("a one", "b two")
	got, err := read(t, cfg, typed("x "),
		[]term.Event{alt('.'), alt('.'), alt('.')}, []term.Event{key(term.KeyEnter)})
	if err != nil || got != "x one" {
		t.Fatalf("wrap = %q, %v", got, err)
	}
	// No history: the buffer is untouched.
	got, err = read(t, muscleCfg(), typed("x "), []term.Event{alt('.')}, []term.Event{key(term.KeyEnter)})
	if err != nil || got != "x " {
		t.Fatalf("empty history = %q, %v", got, err)
	}
}

func TestCommentAcceptParksTheLine(t *testing.T) {
	t.Parallel()

	got, err := read(t, muscleCfg(), typed("rm -rf /important"), []term.Event{alt('#')})
	if err != nil || got != "#rm -rf /important" {
		t.Fatalf("Alt-# = %q, %v", got, err)
	}
}

func TestTransposeChars(t *testing.T) {
	t.Parallel()

	// At end of line, swap the final two characters.
	got, err := read(t, muscleCfg(), typed("hlelo"),
		[]term.Event{key(term.KeyLeft), key(term.KeyLeft), key(term.KeyLeft), ctrl('t')},
		[]term.Event{key(term.KeyEnter)})
	if err != nil || got != "hello" {
		t.Fatalf("Ctrl-T = %q, %v", got, err)
	}
	// Too short to transpose: unchanged.
	got, err = read(t, muscleCfg(), typed("x"), []term.Event{ctrl('t')}, []term.Event{key(term.KeyEnter)})
	if err != nil || got != "x" {
		t.Fatalf("short line = %q, %v", got, err)
	}
}

func TestExternalEditReplacesBuffer(t *testing.T) {
	t.Parallel()

	cfg := muscleCfg()
	var sawText string
	cfg.ExternalEdit = func(text string) (string, bool) {
		sawText = text
		return "edited in $EDITOR", true
	}
	// Ctrl-X Ctrl-E hands the buffer out, takes the result back.
	got, err := read(t, cfg, typed("half a comm"),
		[]term.Event{ctrl('x'), ctrl('e')}, []term.Event{key(term.KeyEnter)})
	if err != nil || got != "edited in $EDITOR" {
		t.Fatalf("external edit = %q, %v", got, err)
	}
	if sawText != "half a comm" {
		t.Errorf("editor received %q", sawText)
	}

	// A declined edit keeps the original text.
	cfg.ExternalEdit = func(string) (string, bool) { return "ignored", false }
	got, err = read(t, cfg, typed("keep me"),
		[]term.Event{ctrl('x'), ctrl('e')}, []term.Event{key(term.KeyEnter)})
	if err != nil || got != "keep me" {
		t.Fatalf("declined edit = %q, %v", got, err)
	}
}

func TestCtrlXChordSwallowsOtherKeys(t *testing.T) {
	t.Parallel()

	// Ctrl-X followed by a non-chord key is a no-op, not an insert.
	got, err := read(t, muscleCfg(), typed("ab"),
		[]term.Event{ctrl('x')}, typed("z"), []term.Event{key(term.KeyEnter)})
	if err != nil || got != "ab" {
		t.Fatalf("chord fallthrough = %q, %v", got, err)
	}
}

func TestOperateAndGetNextQueuesFollowingEntry(t *testing.T) {
	t.Parallel()

	// History newest-first: after running the middle entry with Ctrl-O,
	// the *next newer* one is queued into the following read.
	cfg := muscleCfg("third", "second", "first")
	ed := editor.New(&fakeTerm{events: append(append([]term.Event{},
		key(term.KeyUp), key(term.KeyUp)), ctrl('o'))}, &strings.Builder{}, cfg)
	got, err := ed.ReadCommand(t.Context())
	if err != nil || got != "second" {
		t.Fatalf("Ctrl-O accepted %q, %v", got, err)
	}
}
