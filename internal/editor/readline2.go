package editor

import (
	"unicode"

	"github.com/blairham/koi-shell/internal/term"
)

// Muscle-memory parity round 2 (#118) and numeric arguments (#116).
//
// Round 1 took the bindings whose absence has documented cases of people
// abandoning a shell. These are the rest of readline's emacs keymap: no
// one leaves over a missing Alt-u, but "nothing missing in the first
// hour" eventually means this too, and each is a leaf function against
// machinery that already exists.

// --- numeric arguments (#116) ---
//
// readline lets a count prefix any command: Alt-4 Ctrl-D deletes four
// characters, Alt-3 Alt-d kills three words, Alt-- Alt-. walks last-args
// the other way. The count is accumulated by Alt-<digit> and consumed by
// the next command.
//
// Most commands are repeatable by simply running them again, so the
// dispatcher loops those rather than threading a count through forty
// functions. The few that need the number itself read argCount, which is
// why it is stored rather than only returned.

// numArg accumulates a pending numeric argument. nil means none, which
// is deliberately distinct from 1: `Alt-0 Ctrl-K` is a real thing to
// type, and a command that treats "no argument" as "1" must still be
// able to tell the two apart.
type numArg struct {
	digits   []rune
	negative bool
}

func (a *numArg) value() int {
	n := 0
	for _, d := range a.digits {
		n = n*10 + int(d-'0')
	}
	if len(a.digits) == 0 {
		n = 1 // Alt-- alone means -1, as in readline
	}
	if a.negative {
		return -n
	}
	return n
}

// startArg begins or extends a numeric argument. Reports whether the key
// was part of one.
func (e *Editor) startArg(ev term.KeyEvent) bool {
	if ev.Key != term.KeyRune || ev.Mod != term.ModAlt {
		return false
	}
	switch {
	case ev.Rune >= '0' && ev.Rune <= '9':
		if e.arg == nil {
			e.arg = &numArg{}
		}
		e.arg.digits = append(e.arg.digits, ev.Rune)
		return true
	case ev.Rune == '-' && e.arg == nil:
		e.arg = &numArg{negative: true}
		return true
	}
	return false
}

// consumeArg takes the pending count, defaulting to 1, and leaves it in
// argCount for commands that care about the number or its sign.
func (e *Editor) consumeArg() int {
	n := 1
	if e.arg != nil {
		n = e.arg.value()
		e.arg = nil
	}
	e.argCount = n
	return n
}

// repeatable lists the commands a count applies to by simply running
// them again. Anything not here runs once and may read argCount itself —
// which is the honest split: "do it four times" and "do the fourth one"
// are different commands, and pretending otherwise is how a count ends
// up deleting the wrong thing.
func repeatableBindings() map[binding]bool {
	set := map[binding]bool{
		{key: term.KeyBackspace}:                   true,
		{r: 'h', mod: term.ModCtrl}:                true,
		{key: term.KeyDelete}:                      true,
		{key: term.KeyLeft}:                        true,
		{r: 'b', mod: term.ModCtrl}:                true,
		{key: term.KeyRight}:                       true,
		{r: 'f', mod: term.ModCtrl}:                true,
		{key: term.KeyUp}:                          true,
		{r: 'p', mod: term.ModCtrl}:                true,
		{key: term.KeyDown}:                        true,
		{r: 'n', mod: term.ModCtrl}:                true,
		{r: 'b', mod: term.ModAlt}:                 true,
		{r: 'f', mod: term.ModAlt}:                 true,
		{r: 'd', mod: term.ModAlt}:                 true,
		{key: term.KeyBackspace, mod: term.ModAlt}: true,
		{r: 'w', mod: term.ModCtrl}:                true,
		{r: 't', mod: term.ModCtrl}:                true,
		{r: 'y', mod: term.ModCtrl}:                true,
		{r: 'u', mod: term.ModAlt}:                 true,
		{r: 'l', mod: term.ModAlt}:                 true,
		{r: 'c', mod: term.ModAlt}:                 true,
		{r: 't', mod: term.ModAlt}:                 true,
		{r: '.', mod: term.ModAlt}:                 true,
		{r: '_', mod: term.ModAlt}:                 true,
	}
	// Ctrl-D is repeatable only as delete-forward; on an empty buffer it
	// is EOF, and looping an EOF is meaningless — deleteOrEOF handles the
	// distinction itself.
	set[binding{r: 'd', mod: term.ModCtrl}] = true
	return set
}

// --- case-changing word commands ---

// caseWord applies fn to the word after the cursor and leaves the cursor
// past it: Alt-u (upcase), Alt-l (downcase), Alt-c (capitalize).
func (e *Editor) caseWord(fn func(word []rune) []rune) {
	b := &e.buf
	start := b.cursor
	end := b.wordRight(start)
	if end <= start {
		return
	}
	e.recordUndo()
	word := fn(append([]rune(nil), b.text[start:end]...))
	b.text = append(b.text[:start], append(word, b.text[end:]...)...)
	b.cursor = start + len(word)
}

func (e *Editor) upcaseWord() {
	e.caseWord(func(w []rune) []rune {
		for i, r := range w {
			w[i] = unicode.ToUpper(r)
		}
		return w
	})
}

func (e *Editor) downcaseWord() {
	e.caseWord(func(w []rune) []rune {
		for i, r := range w {
			w[i] = unicode.ToLower(r)
		}
		return w
	})
}

// capitalizeWord upper-cases the first letter of the word and lowers the
// rest, which is readline's behavior — not "upper-case the first
// character of the span", since the span may begin in whitespace.
func (e *Editor) capitalizeWord() {
	e.caseWord(func(w []rune) []rune {
		seen := false
		for i, r := range w {
			switch {
			case !seen && isWordRune(r):
				w[i] = unicode.ToUpper(r)
				seen = true
			case seen:
				w[i] = unicode.ToLower(r)
			}
		}
		return w
	})
}

// transposeWords is Alt-t: swap the word before the cursor with the one
// after it, leaving the cursor past the pair. Ctrl-T does characters;
// this is the one people reach for when two arguments are the wrong way
// round.
func (e *Editor) transposeWords() {
	b := &e.buf
	// Anchor inside or just after the second word, as readline does.
	// wordLeft answers with a word's *start*, so the first word's end has
	// to be found by walking back over the separator — reusing wordLeft
	// for it lands on the start of the wrong word, which is a swap that
	// looks almost right and is not.
	end2 := b.wordRight(b.cursor)
	start2 := b.wordLeft(end2)
	end1 := start2
	for end1 > 0 && !isWordRune(b.text[end1-1]) {
		end1--
	}
	start1 := b.wordLeft(end1)
	if start1 >= end1 || start2 >= end2 || end1 > start2 {
		return
	}
	e.recordUndo()
	text := string(b.text)
	runes := []rune(text)
	first := string(runes[start1:end1])
	middle := string(runes[end1:start2])
	second := string(runes[start2:end2])
	rebuilt := string(runes[:start1]) + second + middle + first + string(runes[end2:])
	b.Set(rebuilt, start1+len([]rune(second))+len([]rune(middle))+len([]rune(first)))
}

// --- quoted insert ---

// startQuotedInsert is Ctrl-V (and Ctrl-Q): the next key is inserted
// literally. It is the only way to type a control character or a leading
// Tab — which is not exotic, it is how anyone types a real tab into a
// `find -printf` or a sed script without the completion engine eating it.
func (e *Editor) startQuotedInsert() { e.pendingQuoted = true }

// quotedInsert converts a key event back into the character the terminal
// sent, so a control chord inserts its control byte rather than running
// its command.
func (e *Editor) quotedInsert(ev term.KeyEvent) {
	e.pendingQuoted = false
	var r rune
	switch {
	case ev.Key == term.KeyTab:
		r = '\t'
	case ev.Key == term.KeyEnter:
		r = '\r'
	case ev.Key == term.KeyEscape:
		r = 0x1b
	case ev.Key == term.KeyBackspace:
		r = 0x7f
	case ev.Key == term.KeyRune && ev.Mod == term.ModCtrl:
		// The terminal sent the control byte; the decoder named it.
		switch {
		case ev.Rune >= 'a' && ev.Rune <= 'z':
			r = ev.Rune - 'a' + 1
		case ev.Rune == '@' || ev.Rune == ' ':
			r = 0
		default:
			r = ev.Rune
		}
	case ev.Key == term.KeyRune:
		r = ev.Rune
	default:
		return
	}
	e.recordUndo()
	e.buf.Insert(string(r))
}

// --- history extremes and revert ---

// revertLine is Alt-r: throw away every edit made to a recalled history
// entry and put the original back. Distinct from undo, which walks one
// change at a time — this is "I have made a mess of this recalled line".
func (e *Editor) revertLine() {
	if e.histPos < 0 || e.hist() == nil {
		// Not on a history line: readline reverts to the line as it was
		// when editing began, which for us is the undo stack's bottom.
		if original, ok := e.undo.bottom(); ok {
			e.buf.Set(original.text, len(original.text))
		}
		return
	}
	if cmd, ok := e.hist().Match(e.histPrefix, e.histPos); ok {
		e.buf.Set(cmd, len([]rune(cmd)))
	}
}

// beginningOfHistory is Alt-<: the oldest entry matching the current
// filter. The store answers by index, so the oldest is found by walking
// until it stops answering — histories are bounded and this is one
// keystroke, not a per-keystroke path.
func (e *Editor) beginningOfHistory() {
	if e.hist() == nil {
		return
	}
	if e.histPos < 0 {
		e.histPending = e.buf.String()
		e.histPrefix = e.histPending
	}
	last, lastN := "", -1
	for n := e.histPos + 1; ; n++ {
		cmd, ok := e.hist().Match(e.histPrefix, n)
		if !ok {
			break
		}
		last, lastN = cmd, n
	}
	if lastN >= 0 {
		e.histPos = lastN
		e.buf.Set(last, len([]rune(last)))
	}
}

// endOfHistory is Alt->: back to the line being typed.
func (e *Editor) endOfHistory() {
	if e.histPos < 0 {
		return
	}
	e.histPos = -1
	e.buf.Set(e.histPending, len([]rune(e.histPending)))
}

// --- character search ---

// startCharSearch is Ctrl-]: move to the next occurrence of the next
// character typed. Alt-Ctrl-] searches backward.
func (e *Editor) startCharSearch(backward bool) {
	e.pendingCharSearch = true
	e.charSearchBack = backward
}

func (e *Editor) charSearch(ev term.KeyEvent) {
	e.pendingCharSearch = false
	if ev.Key != term.KeyRune {
		return
	}
	b := &e.buf
	n := max(e.argCount, 1)
	i := b.cursor
	for ; n > 0; n-- {
		if e.charSearchBack {
			j := i - 1
			for j >= 0 && b.text[j] != ev.Rune {
				j--
			}
			if j < 0 {
				return
			}
			i = j
		} else {
			j := i + 1
			for j < len(b.text) && b.text[j] != ev.Rune {
				j++
			}
			if j >= len(b.text) {
				return
			}
			i = j
		}
	}
	b.cursor = i
}

// --- forward history search ---

// startForwardSearch is Ctrl-S. It is bound at all because internal/term
// enters raw mode through MakeRaw, which clears IXON — so the terminal's
// flow control is not eating the key, which is the reason everyone
// assumes this binding is gone.
func (e *Editor) startForwardSearch() {
	if e.hist() == nil {
		return
	}
	if e.search.active {
		e.searchStep(max(e.search.n-1, 0))
		return
	}
	e.search = searchState{
		active:   true,
		forward:  true,
		saved:    e.buf.String(),
		savedCur: e.buf.Cursor(),
	}
}
