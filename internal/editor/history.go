package editor

import (
	"fmt"

	"github.com/blairham/gish/internal/term"
)

// History is the editor's view of command history. n counts distinct
// matches from most recent (n=0); ok reports exhaustion. The concrete
// store lives in internal/history — this interface keeps the editor
// testable and the dependency one-directional.
type History interface {
	// Match returns the nth most-recent command starting with prefix.
	Match(prefix string, n int) (string, bool)
	// Search returns the nth most-recent command containing query.
	Search(query string, n int) (string, bool)
}

// --- up/down navigation ---
//
// Prefix-aware, zsh-style: the text in the buffer when navigation starts
// becomes the filter, and the pending (unsubmitted) line is restored when
// stepping back below the newest entry. Inside a multi-line buffer,
// arrows move between lines first and only fall through to history at
// the edges. Any buffer edit (recordUndo) resets navigation.

func (e *Editor) historyUp() {
	if e.buf.MoveUp() {
		return
	}
	if e.hist() == nil {
		return
	}
	if e.histPos < 0 {
		e.histPending = e.buf.String()
		e.histPrefix = e.histPending
	}
	if cmd, ok := e.hist().Match(e.histPrefix, e.histPos+1); ok {
		e.histPos++
		e.buf.Set(cmd, len([]rune(cmd)))
	}
}

func (e *Editor) historyDown() {
	if e.buf.MoveDown() {
		return
	}
	if e.hist() == nil || e.histPos < 0 {
		return
	}
	e.histPos--
	if e.histPos < 0 {
		e.buf.Set(e.histPending, len([]rune(e.histPending)))
		return
	}
	if cmd, ok := e.hist().Match(e.histPrefix, e.histPos); ok {
		e.buf.Set(cmd, len([]rune(cmd)))
	}
}

func (e *Editor) hist() History { return e.cfg.History }

// --- ctrl-r incremental search ---

// searchState is the reverse-i-search minor mode. While active, input is
// routed to searchDispatch instead of the keymap.
type searchState struct {
	active bool
	// forward is Ctrl-S: the same incremental search walking toward
	// newer entries. It shares every other piece of the state, because
	// it is the same search — only the step direction differs.
	forward  bool
	query    []rune
	n        int    // which match of query is shown
	saved    string // buffer before the search began
	savedCur int
	failing  bool
}

func (e *Editor) startSearch() {
	if e.hist() == nil {
		return
	}
	// The full-screen picker (#100) is the modern ctrl-r: it shows
	// metadata and fuzzy-matches. Incremental search remains the
	// fallback for terminals that cannot host it.
	if e.cfg.HistoryPick != nil && !e.search.active {
		e.handoverText(e.cfg.HistoryPick)
		return
	}
	if e.search.active {
		// Ctrl-R inside the search steps to the next older match.
		e.searchStep(e.search.n + 1)
		return
	}
	e.search = searchState{
		active:   true,
		saved:    e.buf.String(),
		savedCur: e.buf.Cursor(),
	}
}

// searchPrompt replaces the first-line prompt while searching.
func (e *Editor) searchPrompt() string {
	state := ""
	if e.search.failing {
		state = "failing "
	}
	direction := "reverse"
	if e.search.forward {
		direction = "i"
	}
	return fmt.Sprintf("(%s%s-search)`%s': ", state, direction, string(e.search.query))
}

// searchStep shows the nth match of the current query, or marks the
// search failing while keeping the last good candidate.
func (e *Editor) searchStep(n int) {
	cmd, ok := e.hist().Search(string(e.search.query), n)
	if !ok {
		e.search.failing = true
		return
	}
	e.search.failing = false
	e.search.n = n
	e.buf.Set(cmd, len([]rune(cmd)))
}

// endSearch leaves search mode. restore puts the pre-search buffer back
// (cancel); otherwise the found command stays for further editing.
func (e *Editor) endSearch(restore bool) {
	if restore {
		e.buf.Set(e.search.saved, e.search.savedCur)
	}
	e.search = searchState{}
}

// searchDispatch handles one event while reverse-i-search is active.
func (e *Editor) searchDispatch(ev term.Event) {
	key, ok := ev.(term.KeyEvent)
	if !ok {
		// Paste or resize: leave search, process the event normally.
		e.endSearch(false)
		e.dispatchEvent(ev)
		return
	}
	switch {
	case key.Key == term.KeyRune && key.Mod == 0:
		e.search.query = append(e.search.query, key.Rune)
		e.searchStep(0)
	case key.Key == term.KeyBackspace && key.Mod == 0:
		if len(e.search.query) > 0 {
			e.search.query = e.search.query[:len(e.search.query)-1]
			e.searchStep(0)
		}
	case key.Key == term.KeyRune && key.Rune == 'r' && key.Mod == term.ModCtrl:
		e.search.forward = false
		e.searchStep(e.search.n + 1)
	case key.Key == term.KeyRune && key.Rune == 's' && key.Mod == term.ModCtrl:
		// Ctrl-S inside a search walks back toward newer matches, and
		// switching direction mid-search is the point of having both.
		e.search.forward = true
		e.searchStep(max(e.search.n-1, 0))
	case key.Key == term.KeyRune && key.Rune == 'g' && key.Mod == term.ModCtrl,
		key.Key == term.KeyRune && key.Rune == 'c' && key.Mod == term.ModCtrl,
		key.Key == term.KeyEscape && key.Mod == 0:
		e.endSearch(true) // cancel: pre-search line comes back
	case key.Key == term.KeyEnter && key.Mod == 0:
		e.endSearch(false)
		e.acceptOrNewline()
	default:
		// Movement or another edit: keep the candidate and let the key
		// act on it.
		e.endSearch(false)
		e.dispatchKey(key)
	}
}
