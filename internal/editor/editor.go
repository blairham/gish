// Package editor implements gish's raw-mode inline line editor: the
// zle-equivalent. It owns the buffer, keymap, kill ring, undo, and the
// diff-based renderer (#1 decision); terminal I/O goes through
// internal/term exclusively.
package editor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/blairham/gish/internal/term"
)

// ErrInterrupted reports that the user canceled the line with Ctrl-C.
var ErrInterrupted = errors.New("editor: interrupted")

// Config configures an Editor.
type Config struct {
	// Prompt precedes the first line; ContPrompt the continuation lines.
	Prompt     string
	ContPrompt string
	// AcceptWhen decides whether Enter submits the buffer (true) or
	// inserts a newline to continue editing (false). nil always submits.
	// The shell wires the parser's incomplete-detection here.
	AcceptWhen func(text string) bool
}

type loopState int

const (
	stateRunning loopState = iota
	stateAccepted
	stateCancelled
	stateEOF
)

// Editor reads commands interactively. Not safe for concurrent use; the
// kill ring persists across ReadCommand calls, buffer and undo do not.
type Editor struct {
	term   term.Terminal
	out    io.Writer
	cfg    Config
	keymap map[binding]func(*Editor)

	buf   Buffer
	kills killRing
	undo  undoStack
	rend  *renderer

	state loopState

	// Coalescing state: consecutive kills merge in the kill ring,
	// consecutive self-inserts share one undo entry, yank-pop only
	// follows a yank.
	lastWasKill, thisKill     bool
	lastWasInsert, thisInsert bool
	lastWasYank, thisYank     bool
	yankStart, yankEnd        int
}

// New creates an editor reading from t and drawing to out (both sides of
// the same terminal).
func New(t term.Terminal, out io.Writer, cfg Config) *Editor {
	return &Editor{
		term:   t,
		out:    out,
		cfg:    cfg,
		keymap: defaultKeymap(),
		rend:   newRenderer(out, 80),
	}
}

// ReadCommand runs one interactive read: it enters raw mode, edits until
// the buffer is accepted, and restores the terminal before returning —
// on every path — so the shell always executes commands in cooked mode.
// Returns ErrInterrupted on Ctrl-C and io.EOF on Ctrl-D of an empty
// buffer.
func (e *Editor) ReadCommand(ctx context.Context) (_ string, err error) {
	restore, err := e.term.EnterRaw()
	if err != nil {
		return "", err
	}
	defer func() {
		if rerr := restore(); rerr != nil && err == nil {
			err = rerr
		}
	}()

	// Input decoding stops with this context: once a command is accepted
	// the shell (or its children) own stdin again.
	evctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := e.term.Events(evctx)
	if err != nil {
		return "", err
	}

	if w, _, serr := e.term.Size(); serr == nil {
		e.rend.setWidth(w)
	}
	e.reset()
	e.render()

	for ev := range events {
		e.dispatch(ev)
		switch e.state {
		case stateAccepted:
			e.buf.MoveEnd()
			e.render()
			e.rend.finish()
			return e.buf.String(), nil
		case stateCancelled:
			e.buf.MoveEnd()
			e.render()
			fmt.Fprint(e.out, "^C")
			e.rend.finish()
			return "", ErrInterrupted
		case stateEOF:
			e.rend.finish()
			return "", io.EOF
		case stateRunning:
			e.render()
		}
	}
	// Event stream ended without a decision: canceled from outside or
	// the input closed underneath us.
	e.rend.finish()
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	return "", io.EOF
}

func (e *Editor) reset() {
	e.buf.Set("", 0)
	e.undo.reset()
	e.state = stateRunning
	e.lastWasKill, e.lastWasInsert, e.lastWasYank = false, false, false
}

func (e *Editor) render() {
	raw := e.buf.Lines()
	lines := make([]string, len(raw))
	for i, l := range raw {
		if i == 0 {
			lines[i] = e.cfg.Prompt + l
		} else {
			lines[i] = e.cfg.ContPrompt + l
		}
	}
	cl, before := e.buf.CursorLine()
	prefix := e.cfg.Prompt
	if cl > 0 {
		prefix = e.cfg.ContPrompt
	}
	e.rend.render(lines, cl, displayWidth(prefix+before))
}

func (e *Editor) dispatch(ev term.Event) {
	e.thisKill, e.thisInsert, e.thisYank = false, false, false

	switch ev := ev.(type) {
	case term.ResizeEvent:
		e.rend.setWidth(ev.Width)
	case term.PasteEvent:
		e.recordUndo()
		e.buf.Insert(normalizeNewlines(ev.Text))
	case term.KeyEvent:
		e.dispatchKey(ev)
	}

	e.lastWasKill = e.thisKill
	e.lastWasInsert = e.thisInsert
	e.lastWasYank = e.thisYank
}

func (e *Editor) dispatchKey(ev term.KeyEvent) {
	if cmd, ok := e.keymap[binding{key: ev.Key, r: ev.Rune, mod: ev.Mod}]; ok {
		cmd(e)
		return
	}
	if ev.Key == term.KeyRune && ev.Mod == 0 {
		e.selfInsert(ev.Rune)
	}
}

// normalizeNewlines converts pasted CRLF/CR line endings to the
// buffer's '\n'.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// recordUndo snapshots the buffer before a mutating command.
func (e *Editor) recordUndo() {
	e.undo.push(&e.buf)
}

// kill routes killed text into the ring, coalescing consecutive kills.
func (e *Editor) kill(text string, prepend bool) {
	if text == "" {
		return
	}
	if e.lastWasKill {
		e.kills.coalesce(text, prepend)
	} else {
		e.kills.push(text)
	}
	e.thisKill = true
}

// --- commands ---

func (e *Editor) selfInsert(r rune) {
	if !e.lastWasInsert {
		e.recordUndo()
	}
	e.buf.Insert(string(r))
	e.thisInsert = true
}

func (e *Editor) acceptOrNewline() {
	text := e.buf.String()
	if e.cfg.AcceptWhen == nil || e.cfg.AcceptWhen(text) {
		e.state = stateAccepted
		return
	}
	e.insertNewline()
}

func (e *Editor) insertNewline() {
	e.recordUndo()
	e.buf.Insert("\n")
}

func (e *Editor) interrupt() { e.state = stateCancelled }

func (e *Editor) deleteBack() {
	if e.buf.Cursor() == 0 {
		return
	}
	e.recordUndo()
	e.buf.DeleteBack()
}

func (e *Editor) deleteForward() {
	e.recordUndo()
	e.buf.DeleteForward()
}

func (e *Editor) deleteOrEOF() {
	if e.buf.Empty() {
		e.state = stateEOF
		return
	}
	e.deleteForward()
}

func (e *Editor) killToLineEnd() {
	e.recordUndo()
	e.kill(e.buf.KillToLineEnd(), false)
}

func (e *Editor) killToLineStart() {
	e.recordUndo()
	e.kill(e.buf.KillToLineStart(), true)
}

func (e *Editor) killToWhitespace() {
	e.recordUndo()
	e.kill(e.buf.KillToWhitespace(), true)
}

func (e *Editor) killWordBack() {
	e.recordUndo()
	e.kill(e.buf.KillWordBack(), true)
}

func (e *Editor) killWordForward() {
	e.recordUndo()
	e.kill(e.buf.KillWordForward(), false)
}

func (e *Editor) yank() {
	text := e.kills.top()
	if text == "" {
		return
	}
	e.recordUndo()
	e.yankStart = e.buf.Cursor()
	e.buf.Insert(text)
	e.yankEnd = e.buf.Cursor()
	e.thisYank = true
}

func (e *Editor) yankPop() {
	if !e.lastWasYank {
		return
	}
	e.recordUndo()
	e.buf.deleteRange(e.yankStart, e.yankEnd)
	e.kills.rotate()
	text := e.kills.top()
	e.buf.Insert(text)
	e.yankEnd = e.yankStart + len([]rune(text))
	e.thisYank = true
}

func (e *Editor) undoCmd() {
	if s, ok := e.undo.pop(); ok {
		e.buf.Set(s.text, s.cursor)
	}
}

func (e *Editor) clearScreen() {
	e.rend.clearScreen()
}

// --- keymap ---

// binding identifies a key chord: special keys by key, rune keys by rune,
// both qualified by modifiers.
type binding struct {
	key term.Key
	r   rune
	mod term.Mod
}

func defaultKeymap() map[binding]func(*Editor) {
	return map[binding]func(*Editor){
		{key: term.KeyEnter}:                   (*Editor).acceptOrNewline,
		{key: term.KeyEnter, mod: term.ModAlt}: (*Editor).insertNewline,
		// LF arrives as Ctrl-J (readline's accept-line); piped-into-pty
		// input and some terminals send it instead of CR.
		{r: 'j', mod: term.ModCtrl}:                (*Editor).acceptOrNewline,
		{r: 'c', mod: term.ModCtrl}:                (*Editor).interrupt,
		{r: 'd', mod: term.ModCtrl}:                (*Editor).deleteOrEOF,
		{key: term.KeyBackspace}:                   (*Editor).deleteBack,
		{r: 'h', mod: term.ModCtrl}:                (*Editor).deleteBack,
		{key: term.KeyBackspace, mod: term.ModAlt}: (*Editor).killWordBack,
		{key: term.KeyDelete}:                      (*Editor).deleteForward,
		{key: term.KeyLeft}:                        func(e *Editor) { e.buf.MoveLeft() },
		{r: 'b', mod: term.ModCtrl}:                func(e *Editor) { e.buf.MoveLeft() },
		{key: term.KeyRight}:                       func(e *Editor) { e.buf.MoveRight() },
		{r: 'f', mod: term.ModCtrl}:                func(e *Editor) { e.buf.MoveRight() },
		{key: term.KeyUp}:                          func(e *Editor) { e.buf.MoveUp() },
		{key: term.KeyDown}:                        func(e *Editor) { e.buf.MoveDown() },
		{key: term.KeyHome}:                        func(e *Editor) { e.buf.MoveLineStart() },
		{r: 'a', mod: term.ModCtrl}:                func(e *Editor) { e.buf.MoveLineStart() },
		{key: term.KeyEnd}:                         func(e *Editor) { e.buf.MoveLineEnd() },
		{r: 'e', mod: term.ModCtrl}:                func(e *Editor) { e.buf.MoveLineEnd() },
		{r: 'b', mod: term.ModAlt}:                 func(e *Editor) { e.buf.MoveWordLeft() },
		{r: 'f', mod: term.ModAlt}:                 func(e *Editor) { e.buf.MoveWordRight() },
		{r: 'k', mod: term.ModCtrl}:                (*Editor).killToLineEnd,
		{r: 'u', mod: term.ModCtrl}:                (*Editor).killToLineStart,
		{r: 'w', mod: term.ModCtrl}:                (*Editor).killToWhitespace,
		{r: 'd', mod: term.ModAlt}:                 (*Editor).killWordForward,
		{r: 'y', mod: term.ModCtrl}:                (*Editor).yank,
		{r: 'y', mod: term.ModAlt}:                 (*Editor).yankPop,
		{r: '_', mod: term.ModCtrl}:                (*Editor).undoCmd,
		{r: '/', mod: term.ModCtrl}:                (*Editor).undoCmd,
		{r: 'l', mod: term.ModCtrl}:                (*Editor).clearScreen,
	}
}
