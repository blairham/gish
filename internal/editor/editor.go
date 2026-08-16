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
	// RPrompt renders right-aligned on the first edit line, one column
	// short of the right edge (zsh's default indent). It hides whenever
	// the typed line would reach it — a right prompt never wraps and
	// never blocks input. Empty disables it.
	RPrompt string
	// AcceptWhen decides whether Enter submits the buffer (true) or
	// inserts a newline to continue editing (false). nil always submits.
	// The shell wires the parser's incomplete-detection here.
	AcceptWhen func(text string) bool
	// History backs up/down navigation and ctrl-r search. nil disables
	// both.
	History History
	// Complete answers Tab: candidates for the word at the cursor. nil
	// disables completion.
	Complete func(text string, cursor int) CompleteResult
	// Highlight returns style spans for the buffer text (parser-driven,
	// shell-side). nil disables highlighting.
	Highlight func(text string) []HighlightSpan
	// Suggest returns the full suggested line for the current buffer
	// prefix (fish-style ghost text). nil disables suggestions.
	Suggest func(text string) string
	// Diagnose returns caution lines for the buffer text (parser-driven
	// footgun warnings, shell-side). They render below the edit line on
	// the completion-candidates surface, advisory only — the editor
	// never blocks acceptance on them. nil disables diagnostics.
	Diagnose func(text string) []string
	// ExternalEdit is Ctrl-X Ctrl-E: edit the buffer in $EDITOR. The
	// editor cedes the terminal (cooked mode, decoder stopped) around
	// the call; ok=false keeps the original text. nil disables it.
	ExternalEdit func(text string) (string, bool)
}

type loopState int

const (
	stateRunning loopState = iota
	stateAccepted
	stateCancelled
	stateEOF
	stateExternalEdit
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
	// follows a yank, repeated Alt-. cycles older last-args.
	lastWasKill, thisKill       bool
	lastWasInsert, thisInsert   bool
	lastWasYank, thisYank       bool
	lastWasYankArg, thisYankArg bool
	yankStart, yankEnd          int
	yankArgStart, yankArgEnd    int
	yankArgN                    int

	// pendingCtrlX arms the Ctrl-X chord for exactly one key.
	pendingCtrlX bool

	// History navigation (see history.go): histPos is the entry shown
	// (-1 = the live pending line), histPrefix the filter captured when
	// navigation began.
	histPos     int
	histPending string
	histPrefix  string
	search      searchState

	// candList shows completion candidates below the edit line for one
	// render cycle; any following event clears it.
	candList []string

	// preload seeds the next ReadCommand's buffer (see Preload).
	preload string
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

// SetPrompt updates both prompts for subsequent ReadCommand calls — the
// shell recomputes them per command (cwd, last exit status).
func (e *Editor) SetPrompt(prompt, contPrompt string) {
	e.cfg.Prompt = prompt
	e.cfg.ContPrompt = contPrompt
}

// SetRPrompt updates the right-side prompt (empty disables it).
func (e *Editor) SetRPrompt(rprompt string) {
	e.cfg.RPrompt = rprompt
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

	if w, _, serr := e.term.Size(); serr == nil {
		e.rend.setWidth(w)
	}
	e.reset()
	e.render()

	for {
		line, done, rerr := e.readEvents(ctx)
		if done {
			return line, rerr
		}
		// External edit (Ctrl-X Ctrl-E): cede the terminal — cooked
		// mode, decoder stopped — run $EDITOR, then take it back.
		if cerr := restore(); cerr != nil {
			return "", cerr
		}
		if text, ok := e.cfg.ExternalEdit(e.buf.String()); ok {
			e.buf.Set(text, len([]rune(text)))
		}
		if restore, err = e.term.EnterRaw(); err != nil {
			restore = func() error { return nil }
			return "", err
		}
		e.state = stateRunning
		e.render()
	}
}

// readEvents runs one decoder session until the buffer is decided
// (done=true) or an external edit is requested (done=false — the
// caller suspends the terminal and calls back in).
func (e *Editor) readEvents(ctx context.Context) (line string, done bool, err error) {
	// Input decoding stops with this context: once a command is accepted
	// the shell (or its children) own stdin again.
	evctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := e.term.Events(evctx)
	if err != nil {
		return "", true, err
	}

	for ev := range events {
		e.dispatch(ev)
		switch e.state {
		case stateAccepted:
			e.buf.MoveEnd()
			e.render()
			e.rend.finish()
			return e.buf.String(), true, nil
		case stateCancelled:
			e.buf.MoveEnd()
			e.render()
			fmt.Fprint(e.out, "^C")
			e.rend.finish()
			return "", true, ErrInterrupted
		case stateEOF:
			e.rend.finish()
			return "", true, io.EOF
		case stateExternalEdit:
			e.rend.finish()
			return "", false, nil
		case stateRunning:
			e.render()
		}
	}
	// Event stream ended without a decision: canceled from outside or
	// the input closed underneath us.
	e.rend.finish()
	if cerr := ctx.Err(); cerr != nil {
		return "", true, cerr
	}
	return "", true, io.EOF
}

// Preload seeds the next ReadCommand's buffer (cursor at the end) —
// how composed text lands for review instead of executing (#20).
func (e *Editor) Preload(text string) {
	e.preload = text
}

func (e *Editor) reset() {
	e.buf.Set(e.preload, len(e.preload))
	e.preload = ""
	e.undo.reset()
	e.state = stateRunning
	e.lastWasKill, e.lastWasInsert, e.lastWasYank = false, false, false
	e.histPos = -1
	e.search = searchState{}
}

func (e *Editor) render() {
	firstPrompt := e.cfg.Prompt
	if e.search.active {
		firstPrompt = e.searchPrompt()
	}
	// Multi-line prompts (the themed two-line layout) split into banner
	// lines drawn above the edit line and the prefix of the edit line
	// itself.
	banner, linePrefix := promptParts(firstPrompt)

	raw := e.buf.Lines()
	styled := raw
	if e.cfg.Highlight != nil && !e.search.active {
		styled = applyHighlight(raw, e.cfg.Highlight(e.buf.String()))
	}
	if ghost := e.ghostText(); ghost != "" {
		styled = append(styled[:len(styled)-1:len(styled)-1],
			styled[len(styled)-1]+"\x1b[2m"+ghost+styleReset)
	}
	lines := make([]string, 0, len(banner)+len(styled))
	lines = append(lines, banner...)
	for i, l := range styled {
		if i == 0 {
			lines = append(lines, linePrefix+l)
		} else {
			lines = append(lines, e.cfg.ContPrompt+l)
		}
	}
	if e.cfg.RPrompt != "" && !e.search.active {
		idx := len(banner)
		lines[idx] = withRPrompt(lines[idx], e.cfg.RPrompt, e.rend.width)
	}
	cl, before := e.buf.CursorLine()
	prefix := linePrefix
	if cl > 0 {
		prefix = e.cfg.ContPrompt
	}
	lines = append(lines, candidateRows(e.candList, e.rend.width)...)
	// Diagnostics share the candidate surface (candidates win for their
	// one-event lifetime) and vanish from the final accepted render so
	// scrollback stays clean.
	if e.state == stateRunning && len(e.candList) == 0 && e.cfg.Diagnose != nil && !e.search.active {
		lines = append(lines, e.cfg.Diagnose(e.buf.String())...)
	}
	e.rend.render(lines, len(banner)+cl, displayWidth(prefix+before))
}

// promptParts splits a possibly multi-line prompt into the banner lines
// above the edit line and the edit line's prefix.
func promptParts(prompt string) (banner []string, prefix string) {
	parts := strings.Split(prompt, "\n")
	return parts[:len(parts)-1], parts[len(parts)-1]
}

func (e *Editor) dispatch(ev term.Event) {
	e.thisKill, e.thisInsert, e.thisYank, e.thisYankArg = false, false, false, false
	e.candList = nil // completion lists live for exactly one event

	if e.search.active {
		e.searchDispatch(ev)
	} else {
		e.dispatchEvent(ev)
	}

	e.lastWasKill = e.thisKill
	e.lastWasInsert = e.thisInsert
	e.lastWasYank = e.thisYank
	e.lastWasYankArg = e.thisYankArg
}

func (e *Editor) dispatchEvent(ev term.Event) {
	switch ev := ev.(type) {
	case term.ResizeEvent:
		e.rend.setWidth(ev.Width)
	case term.PasteEvent:
		e.recordUndo()
		e.buf.Insert(normalizeNewlines(ev.Text))
	case term.KeyEvent:
		e.dispatchKey(ev)
	}
}

func (e *Editor) dispatchKey(ev term.KeyEvent) {
	if e.pendingCtrlX {
		e.pendingCtrlX = false
		if ev.Key == term.KeyRune && ev.Rune == 'e' && ev.Mod == term.ModCtrl {
			e.externalEditRequest()
		}
		return
	}
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

// recordUndo snapshots the buffer before a mutating command. Any edit
// also ends history navigation: the next up-arrow re-captures its prefix
// from the edited line.
func (e *Editor) recordUndo() {
	e.undo.push(&e.buf)
	e.histPos = -1
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
		{key: term.KeyTab}:                     (*Editor).completeTab,
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
		{key: term.KeyRight}:                       (*Editor).moveRightOrAccept,
		{r: 'f', mod: term.ModCtrl}:                (*Editor).moveRightOrAccept,
		{key: term.KeyUp}:                          (*Editor).historyUp,
		{r: 'p', mod: term.ModCtrl}:                (*Editor).historyUp,
		{key: term.KeyDown}:                        (*Editor).historyDown,
		{r: 'n', mod: term.ModCtrl}:                (*Editor).historyDown,
		{r: 'r', mod: term.ModCtrl}:                (*Editor).startSearch,
		{key: term.KeyHome}:                        func(e *Editor) { e.buf.MoveLineStart() },
		{r: 'a', mod: term.ModCtrl}:                func(e *Editor) { e.buf.MoveLineStart() },
		{key: term.KeyEnd}:                         (*Editor).moveLineEndOrAccept,
		{r: 'e', mod: term.ModCtrl}:                (*Editor).moveLineEndOrAccept,
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
		// The muscle-memory set (#96).
		{r: '.', mod: term.ModAlt}:  (*Editor).yankLastArg,
		{r: '_', mod: term.ModAlt}:  (*Editor).yankLastArg,
		{r: '#', mod: term.ModAlt}:  (*Editor).commentAccept,
		{r: 't', mod: term.ModCtrl}: (*Editor).transposeChars,
		{r: 'o', mod: term.ModCtrl}: (*Editor).operateAndGetNext,
		{r: 'x', mod: term.ModCtrl}: (*Editor).startCtrlX,
	}
}
