package editor

import (
	"strings"
	"unicode"

	"github.com/blairham/gish/internal/term"
)

// Vi mode (#163).
//
// "I actually had to stop using fish and go back to zsh because I
// couldn't get past the lack of vi support" — and fish's own maintainer
// concedes its vi mode is far from perfect. The corpus is unambiguous
// that a partial vi mode is its own abandonment cause: someone who
// types `ciw` and gets three inserted characters is worse off than
// someone who was told there is no vi mode at all.
//
// So the shape here is operators × motions × text objects, resolved the
// way vi resolves them, rather than a table of the dozen commands that
// are easiest to hardcode. That is what makes `d2w`, `c3aw`, `y%`-class
// composition work without a case for each pairing — and what keeps the
// gaps honest: what is missing is missing uniformly (visual mode,
// registers, marks, replace mode), not scattered through the pairs.
//
// Insert mode deliberately keeps the emacs keymap. readline's vi-insert
// is nearly bare, but a shell is not vi: nobody hits Escape to reach the
// start of a line they are still typing, and Ctrl-A costing a mode
// switch is a papercut in its own right. Control chords fall through in
// normal mode too, which is how Ctrl-C, Ctrl-R and Ctrl-L keep working
// without being rebound per mode.

// EditMode selects the editor's keymap dialect.
type EditMode int

const (
	// ModeEmacs is the default: readline's emacs keymap.
	ModeEmacs EditMode = iota
	// ModeVi starts each line in vi insert mode, with Escape to normal.
	ModeVi
)

type viMode int

const (
	viInsert viMode = iota
	viNormal
)

// Cursor shapes (DECSCUSR). A vi user reads the mode off the cursor
// before they read it off anything else, and a terminal that does not
// implement the sequence ignores it — it is zero-width either way, so
// the renderer's row model is untouched.
const (
	viCursorBlock = "\x1b[2 q"
	viCursorBar   = "\x1b[6 q"
)

// viState is the modal layer's whole memory. It lives for one
// ReadCommand: a new line starts in insert mode, as in bash and zsh.
type viState struct {
	mode viMode

	count     int  // accumulated count prefix (0 = none)
	pendingOp rune // 'd', 'c' or 'y' awaiting a motion or object
	opCount   int  // count that preceded the operator

	pendingFind    rune // 'f', 'F', 't' or 'T' awaiting its target
	pendingObject  rune // 'i' or 'a' awaiting the object's kind
	pendingReplace bool // r awaiting the replacement character
	pendingG       bool // g awaiting its second key

	lastFind, lastFindTarget rune // for ; and ,

	reg         string // the unnamed register: last delete or yank
	regLinewise bool
}

// viEnabled reports whether the modal layer is active.
func (e *Editor) viEnabled() bool { return e.vi != nil }

// viReset returns to insert mode for a fresh line.
func (e *Editor) viReset() {
	if !e.viEnabled() {
		return
	}
	e.vi.mode = viInsert
	e.viClearPending()
	e.viSetCursor()
}

func (e *Editor) viClearPending() {
	v := e.vi
	v.count, v.opCount = 0, 0
	v.pendingOp, v.pendingFind, v.pendingObject, v.pendingG = 0, 0, 0, false
	v.pendingReplace = false
}

// viSetCursor tells the terminal which mode we are in.
func (e *Editor) viSetCursor() {
	shape := viCursorBar
	if e.vi.mode == viNormal {
		shape = viCursorBlock
	}
	io := e.out
	if io == nil {
		return
	}
	_, _ = io.Write([]byte(shape))
}

// viRestoreCursor asks for the default cursor as the editor gives the
// terminal back. Emitting the bar rather than a reset keeps it one
// sequence and matches what the next line will ask for anyway.
func (e *Editor) viRestoreCursor() {
	if !e.viEnabled() || e.out == nil {
		return
	}
	_, _ = e.out.Write([]byte(viCursorBar))
}

func (e *Editor) viEnterNormal() {
	e.vi.mode = viNormal
	e.viClearPending()
	// In normal mode the cursor sits *on* a character, never past the
	// last one — the same reason Escape in vi moves left.
	if e.buf.cursor > e.buf.lineStart(e.buf.cursor) && e.buf.cursor >= e.buf.lineEnd(e.buf.cursor) {
		e.buf.cursor = prevBoundary(e.buf.text, e.buf.cursor)
	}
	e.viSetCursor()
}

func (e *Editor) viEnterInsert() {
	e.vi.mode = viInsert
	e.viClearPending()
	e.viSetCursor()
}

// viDispatchKey handles one key in normal mode. It reports whether the
// key was consumed; false lets the emacs keymap have it, which is how
// control chords stay bound once instead of twice.
func (e *Editor) viDispatchKey(ev term.KeyEvent) bool {
	v := e.vi
	// Control and alt chords, and the named keys, are not vi commands.
	// Escape cancels whatever was half-typed, which is what a vi user
	// presses when they have lost track of the state.
	if ev.Key == term.KeyEscape {
		e.viClearPending()
		return true
	}
	if ev.Key != term.KeyRune || ev.Mod != 0 {
		return false
	}
	r := ev.Rune

	switch {
	case v.pendingReplace:
		e.viReplaceChar(r)
		return true
	case v.pendingFind != 0:
		cmd := v.pendingFind
		v.pendingFind = 0
		e.viDoFind(cmd, r)
		return true
	case v.pendingObject != 0:
		kind := v.pendingObject
		v.pendingObject = 0
		e.viDoTextObject(kind, r)
		return true
	case v.pendingG:
		v.pendingG = false
		if r == 'g' {
			e.viApplyMotion(viMotion{to: 0, valid: true, linewise: true})
		}
		return true
	}

	// Counts. A leading 0 is the motion, not a digit; a later 0 is.
	if r >= '1' && r <= '9' || (r == '0' && v.count > 0) {
		v.count = v.count*10 + int(r-'0')
		return true
	}

	return e.viCommand(r)
}

// viCount consumes the pending count, defaulting to 1. When an operator
// is pending, vi multiplies the two counts (2d3w deletes six words).
func (e *Editor) viCount() int {
	v := e.vi
	n := max(v.count, 1)
	if v.opCount > 0 {
		n *= v.opCount
	}
	v.count, v.opCount = 0, 0
	return n
}

func (e *Editor) viCommand(r rune) bool {
	v := e.vi

	// Operators. Doubling one (dd, cc, yy) is the linewise form.
	if r == 'd' || r == 'c' || r == 'y' {
		if v.pendingOp == r {
			n := e.viCount()
			e.viOperate(r, e.viLinewiseRange(n), true)
			return true
		}
		v.pendingOp = r
		v.opCount = v.count
		v.count = 0
		return true
	}

	// `cw` is `ce`. vi's oldest special case: on a non-blank, cw changes
	// to the end of the word and leaves the space after it, because
	// otherwise every `cw` would join the next word onto the one you are
	// retyping. People have this in their fingers; matching it matters
	// more than the rule it breaks.
	if (r == 'w' || r == 'W') && v.pendingOp == 'c' {
		b := &e.buf
		if b.cursor < len(b.text) && !unicode.IsSpace(b.text[b.cursor]) {
			n := e.viCount()
			i := b.cursor
			for ; n > 0; n-- {
				i = viWordEnd(b.text, i, r == 'W')
			}
			e.viApplyMotion(viMotion{to: i, inclusive: true, valid: true})
			return true
		}
	}

	// Text objects are only meaningful under an operator.
	if (r == 'i' || r == 'a') && v.pendingOp != 0 {
		v.pendingObject = r
		return true
	}

	if m, ok := e.viResolveMotion(r); ok {
		e.viApplyMotion(m)
		return true
	}
	if e.viAction(r) {
		return true
	}
	// An unbound printable key in normal mode is a no-op, never a
	// character. vi beeps here; a shell that typed it instead would put
	// stray letters into the command line of anyone who mistimed Escape,
	// which is the worst possible failure for a mode that exists to make
	// editing safe.
	e.viClearPending()
	return true
}

// viMotion is a resolved motion: where it lands, and how an operator
// should treat the span between here and there.
type viMotion struct {
	to        int
	inclusive bool // the character at `to` is part of an operator's span
	linewise  bool
	valid     bool
}

// viResolveMotion resolves the pure motions. ok=false means r is not a
// motion, so the caller can try it as an action.
func (e *Editor) viResolveMotion(r rune) (viMotion, bool) {
	b := &e.buf
	v := e.vi
	switch r {
	case 'h':
		n := e.viCount()
		i := b.cursor
		for ; n > 0; n-- {
			if i <= b.lineStart(i) {
				break
			}
			i = prevBoundary(b.text, i)
		}
		return viMotion{to: i, valid: true}, true
	case 'l', ' ':
		n := e.viCount()
		i := b.cursor
		for ; n > 0; n-- {
			if i >= b.lineEnd(i) {
				break
			}
			i = nextBoundary(b.text, i)
		}
		return viMotion{to: i, valid: true}, true
	case '0':
		e.viCount()
		return viMotion{to: b.lineStart(b.cursor), valid: true}, true
	case '^':
		e.viCount()
		i := b.lineStart(b.cursor)
		for i < b.lineEnd(i) && unicode.IsSpace(b.text[i]) {
			i++
		}
		return viMotion{to: i, valid: true}, true
	case '$':
		e.viCount()
		return viMotion{to: b.lineEnd(b.cursor), valid: true}, true
	case 'w', 'W':
		n := e.viCount()
		i := b.cursor
		for ; n > 0; n-- {
			i = viWordForward(b.text, i, r == 'W')
		}
		return viMotion{to: i, valid: true}, true
	case 'b', 'B':
		n := e.viCount()
		i := b.cursor
		for ; n > 0; n-- {
			i = viWordBack(b.text, i, r == 'B')
		}
		return viMotion{to: i, valid: true}, true
	case 'e', 'E':
		n := e.viCount()
		i := b.cursor
		for ; n > 0; n-- {
			i = viWordEnd(b.text, i, r == 'E')
		}
		return viMotion{to: i, inclusive: true, valid: true}, true
	case 'f', 'F', 't', 'T':
		// The target character has not arrived yet; the count survives
		// until it does. Consumed either way — a half-typed find must
		// never reach the fallback keymap and type an "f".
		v.pendingFind = r
		return viMotion{}, true
	case ';', ',':
		if v.lastFind == 0 {
			return viMotion{}, true // nothing to repeat: consumed, no-op
		}
		cmd := v.lastFind
		if r == ',' {
			cmd = viFlipFind(cmd)
		}
		return e.viFindMotion(cmd, v.lastFindTarget), true
	case 'G':
		n := v.count
		e.viCount()
		if n == 0 {
			return viMotion{to: len(b.text), linewise: true, valid: true}, true
		}
		return viMotion{to: viLineIndex(b, n), linewise: true, valid: true}, true
	case 'g':
		v.pendingG = true
		return viMotion{}, true
	}
	return viMotion{}, false
}

// viLineIndex returns the index of the start of logical line n (1-based).
func viLineIndex(b *Buffer, n int) int {
	i, line := 0, 1
	for i < len(b.text) && line < n {
		if b.text[i] == '\n' {
			line++
		}
		i++
	}
	return i
}

func viFlipFind(cmd rune) rune {
	switch cmd {
	case 'f':
		return 'F'
	case 'F':
		return 'f'
	case 't':
		return 'T'
	default:
		return 't'
	}
}

// viDoFind completes an f/F/t/T once the target character arrives.
func (e *Editor) viDoFind(cmd, target rune) {
	e.vi.lastFind, e.vi.lastFindTarget = cmd, target
	e.viApplyMotion(e.viFindMotion(cmd, target))
}

func (e *Editor) viFindMotion(cmd, target rune) viMotion {
	b := &e.buf
	n := e.viCount()
	i := b.cursor
	forward := cmd == 'f' || cmd == 't'
	for ; n > 0; n-- {
		if forward {
			j := i + 1
			for j < b.lineEnd(i) && b.text[j] != target {
				j++
			}
			if j >= b.lineEnd(i) {
				return viMotion{} // not found: the whole command is a no-op
			}
			i = j
		} else {
			j := i - 1
			start := b.lineStart(i)
			for j >= start && b.text[j] != target {
				j--
			}
			if j < start {
				return viMotion{}
			}
			i = j
		}
	}
	switch cmd {
	case 't':
		i--
	case 'T':
		i++
	}
	return viMotion{to: i, inclusive: forward, valid: true}
}

// viApplyMotion either moves the cursor or, with an operator pending,
// applies that operator to the span the motion covers.
func (e *Editor) viApplyMotion(m viMotion) {
	if !m.valid {
		// A motion that resolved to nothing (an f with no match) is a
		// no-op, and so is one that is still waiting for its second key.
		// Neither may cancel an operator that is legitimately pending.
		if e.vi.pendingFind == 0 && !e.vi.pendingG {
			e.viClearPending()
		}
		return
	}
	v := e.vi
	if v.pendingOp == 0 {
		e.buf.cursor = min(max(m.to, 0), len(e.buf.text))
		e.viClampCursor()
		return
	}
	op := v.pendingOp
	from, to := e.buf.cursor, m.to
	if from > to {
		from, to = to, from
	} else if m.inclusive {
		to = nextBoundary(e.buf.text, to)
	}
	if m.linewise {
		from, to = e.buf.lineStart(from), e.buf.lineEnd(to)
		if to < len(e.buf.text) {
			to++
		}
	}
	e.viOperate(op, [2]int{from, to}, m.linewise)
}

// viLinewiseRange is the span of n whole lines from the cursor's line —
// the dd/cc/yy form.
func (e *Editor) viLinewiseRange(n int) [2]int {
	b := &e.buf
	from := b.lineStart(b.cursor)
	to := from
	for ; n > 0; n-- {
		to = b.lineEnd(to)
		if to < len(b.text) {
			to++
		}
	}
	return [2]int{from, to}
}

// viOperate performs d/c/y over [from,to).
func (e *Editor) viOperate(op rune, span [2]int, linewise bool) {
	from, to := max(span[0], 0), min(span[1], len(e.buf.text))
	e.viClearPending()
	if from >= to {
		return
	}
	text := string(e.buf.text[from:to])
	e.vi.reg, e.vi.regLinewise = text, linewise
	if op == 'y' {
		e.buf.cursor = from
		return
	}
	e.recordUndo()
	// The kill ring gets it too: a vi user who yanks with `d` and then
	// reaches for Ctrl-Y should not find it empty.
	e.kill(text, false)
	e.buf.deleteRange(from, to)
	e.buf.cursor = min(from, len(e.buf.text))
	if op == 'c' {
		// cc leaves an empty line to type on rather than joining the
		// next one up — but only when there was a line ending to keep.
		// On a one-line buffer the whole span is the line, and putting
		// a newline back would submit an empty second line.
		if linewise && strings.HasSuffix(text, "\n") {
			e.buf.Insert("\n")
			e.buf.cursor--
		}
		e.viEnterInsert()
		return
	}
	e.viClampCursor()
}

// viDoTextObject resolves iw/aw and the quote and bracket pairs.
func (e *Editor) viDoTextObject(kind, object rune) {
	v := e.vi
	op := v.pendingOp
	n := e.viCount()
	span, ok := e.viObjectSpan(kind, object, n)
	if !ok || op == 0 {
		e.viClearPending()
		return
	}
	e.viOperate(op, span, false)
}

// viObjectSpan computes a text object's span. `i` is the inner object,
// `a` includes its delimiters (or, for words, the trailing whitespace).
func (e *Editor) viObjectSpan(kind, object rune, n int) ([2]int, bool) {
	b := &e.buf
	switch object {
	case 'w', 'W':
		big := object == 'W'
		start := viWordStartAt(b.text, b.cursor, big)
		end := b.cursor
		for ; n > 0; n-- {
			end = viWordEnd(b.text, max(end, start), big)
			end = nextBoundary(b.text, end)
		}
		if kind == 'a' {
			for end < len(b.text) && b.text[end] != '\n' && unicode.IsSpace(b.text[end]) {
				end++
			}
		}
		return [2]int{start, end}, true
	case '"', '\'', '`':
		return viPairSpan(b, object, object, kind == 'a')
	case '(', ')', 'b':
		return viPairSpan(b, '(', ')', kind == 'a')
	case '{', '}', 'B':
		return viPairSpan(b, '{', '}', kind == 'a')
	case '[', ']':
		return viPairSpan(b, '[', ']', kind == 'a')
	case '<', '>':
		return viPairSpan(b, '<', '>', kind == 'a')
	}
	return [2]int{}, false
}

// viPairSpan finds the delimiters around the cursor. For identical
// delimiters (quotes) it scans from the start of the line, which is what
// makes `ci"` work with the cursor anywhere inside — including on a
// quote itself.
func viPairSpan(b *Buffer, open, close rune, around bool) ([2]int, bool) {
	if open == close {
		lineStart, lineEnd := b.lineStart(b.cursor), b.lineEnd(b.cursor)
		var positions []int
		for i := lineStart; i < lineEnd; i++ {
			if b.text[i] == open && (i == 0 || b.text[i-1] != '\\') {
				positions = append(positions, i)
			}
		}
		for i := 0; i+1 < len(positions); i += 2 {
			l, r := positions[i], positions[i+1]
			if b.cursor <= r {
				if around {
					return [2]int{l, r + 1}, true
				}
				return [2]int{l + 1, r}, true
			}
		}
		return [2]int{}, false
	}

	// Distinct delimiters: walk out, counting depth in both directions.
	l, depth := b.cursor, 0
	if b.cursor < len(b.text) && b.text[b.cursor] == open {
		// The cursor is on the opener; it is our left edge.
	} else {
		for ; l >= 0; l-- {
			if b.text[l] == close && l != b.cursor {
				depth++
			} else if b.text[l] == open {
				if depth == 0 {
					break
				}
				depth--
			}
		}
	}
	if l < 0 || l >= len(b.text) || b.text[l] != open {
		return [2]int{}, false
	}
	r, depth := l+1, 0
	for ; r < len(b.text); r++ {
		if b.text[r] == open {
			depth++
		} else if b.text[r] == close {
			if depth == 0 {
				break
			}
			depth--
		}
	}
	if r >= len(b.text) {
		return [2]int{}, false
	}
	if around {
		return [2]int{l, r + 1}, true
	}
	return [2]int{l + 1, r}, true
}

// viAction handles the commands that are not motions.
func (e *Editor) viAction(r rune) bool {
	b := &e.buf
	v := e.vi
	// An operator waiting on a motion cannot be satisfied by an action.
	if v.pendingOp != 0 && strings.ContainsRune("iaIAoOxXDCYpPr~us/jk\r", r) {
		e.viClearPending()
		return true
	}
	switch r {
	case 'i':
		e.viEnterInsert()
	case 'I':
		b.MoveLineStart()
		e.viEnterInsert()
	case 'a':
		if b.cursor < b.lineEnd(b.cursor) {
			b.MoveRight()
		}
		e.viEnterInsert()
	case 'A':
		b.MoveLineEnd()
		e.viEnterInsert()
	case 'o':
		e.recordUndo()
		b.MoveLineEnd()
		b.Insert("\n")
		e.viEnterInsert()
	case 'O':
		e.recordUndo()
		b.MoveLineStart()
		b.Insert("\n")
		b.cursor--
		e.viEnterInsert()
	case 'x':
		n := e.viCount()
		e.recordUndo()
		end := b.cursor
		for ; n > 0 && end < b.lineEnd(b.cursor); n-- {
			end = nextBoundary(b.text, end)
		}
		v.reg = b.deleteRange(b.cursor, end)
		e.viClampCursor()
	case 'X':
		n := e.viCount()
		e.recordUndo()
		start := b.cursor
		for ; n > 0 && start > b.lineStart(b.cursor); n-- {
			start = prevBoundary(b.text, start)
		}
		v.reg = b.deleteRange(start, b.cursor)
	case 'D':
		e.viCount()
		e.recordUndo()
		v.reg = b.deleteRange(b.cursor, b.lineEnd(b.cursor))
		e.viClampCursor()
	case 'C':
		e.viCount()
		e.recordUndo()
		v.reg = b.deleteRange(b.cursor, b.lineEnd(b.cursor))
		e.viEnterInsert()
	case 'Y':
		e.viCount()
		span := e.viLinewiseRange(1)
		v.reg, v.regLinewise = string(b.text[span[0]:min(span[1], len(b.text))]), true
	case 's':
		n := e.viCount()
		e.recordUndo()
		end := b.cursor
		for ; n > 0 && end < b.lineEnd(b.cursor); n-- {
			end = nextBoundary(b.text, end)
		}
		v.reg = b.deleteRange(b.cursor, end)
		e.viEnterInsert()
	case 'S':
		e.viCount()
		e.recordUndo()
		span := e.viLinewiseRange(1)
		v.reg, v.regLinewise = b.deleteRange(span[0], min(span[1], len(b.text))), true
		if strings.HasSuffix(v.reg, "\n") {
			b.Insert("\n")
			b.cursor--
		}
		e.viEnterInsert()
	case 'p', 'P':
		e.viPaste(r == 'p')
	case 'r':
		v.pendingReplace = true
	case '~':
		n := e.viCount()
		e.recordUndo()
		for ; n > 0 && b.cursor < b.lineEnd(b.cursor); n-- {
			c := b.text[b.cursor]
			if unicode.IsUpper(c) {
				b.text[b.cursor] = unicode.ToLower(c)
			} else {
				b.text[b.cursor] = unicode.ToUpper(c)
			}
			b.cursor = nextBoundary(b.text, b.cursor)
		}
		e.viClampCursor()
	case 'u':
		e.viCount()
		e.undoCmd()
		e.viClampCursor()
	case 'j':
		e.viCount()
		e.viDownOrHistory()
	case 'k':
		e.viCount()
		e.viUpOrHistory()
	case '/':
		e.viCount()
		e.startSearch()
	case 'v':
		// Visual mode is not implemented; `v` in bash's vi mode opens the
		// line in $EDITOR, which is both more useful and already built.
		e.viCount()
		e.externalEditRequest()
	default:
		return false
	}
	return true
}

// viPaste inserts the register after (p) or before (P) the cursor.
func (e *Editor) viPaste(after bool) {
	v := e.vi
	n := e.viCount()
	if v.reg == "" {
		return
	}
	e.recordUndo()
	b := &e.buf
	text := strings.Repeat(v.reg, n)
	switch {
	case v.regLinewise:
		if after {
			b.cursor = b.lineEnd(b.cursor)
			if b.cursor < len(b.text) {
				b.cursor++
			} else {
				text = "\n" + strings.TrimSuffix(text, "\n")
			}
		} else {
			b.MoveLineStart()
		}
		at := b.cursor
		b.Insert(text)
		b.cursor = at
	default:
		if after && b.cursor < b.lineEnd(b.cursor) {
			b.MoveRight()
		}
		b.Insert(text)
		e.buf.cursor = prevBoundary(b.text, b.cursor)
	}
	e.viClampCursor()
}

// viReplaceChar is `r`: overwrite the character under the cursor.
func (e *Editor) viReplaceChar(r rune) {
	e.vi.pendingReplace = false
	n := e.viCount()
	b := &e.buf
	if b.cursor+n > b.lineEnd(b.cursor) {
		return // vi refuses rather than replacing fewer than asked
	}
	e.recordUndo()
	for i := 0; i < n; i++ {
		b.text[b.cursor+i] = r
	}
	b.cursor += n - 1
	e.viClampCursor()
}

// viUpOrHistory: k walks history on a single-line buffer, and moves by
// line on a multi-line one.
//
// bash binds k to history outright because its buffer is one line. Ours
// is not — a multi-line buffer is normal here (a for loop, a pasted
// heredoc), and in one, "up" plainly means the line above. Preferring
// history there would make the buffer uneditable by exactly the person
// who reached for vi mode to edit it.
func (e *Editor) viUpOrHistory() {
	if strings.Contains(string(e.buf.text), "\n") {
		e.buf.MoveUp()
		e.viClampCursor()
		return
	}
	e.historyUp()
	e.viClampCursor()
}

func (e *Editor) viDownOrHistory() {
	if strings.Contains(string(e.buf.text), "\n") {
		e.buf.MoveDown()
		e.viClampCursor()
		return
	}
	e.historyDown()
	e.viClampCursor()
}

// viClampCursor keeps the cursor on a character in normal mode.
func (e *Editor) viClampCursor() {
	if !e.viEnabled() || e.vi.mode != viNormal {
		return
	}
	b := &e.buf
	if end := b.lineEnd(b.cursor); b.cursor >= end && end > b.lineStart(b.cursor) {
		b.cursor = prevBoundary(b.text, end)
	}
}

// --- word motions ---
//
// vi has two word notions: "word" (runs of word characters, with runs of
// punctuation counting as words of their own) and "WORD" (runs of
// non-whitespace). Both matter in a shell — `dw` on `--flag=value`
// stopping at the `=` is the behavior people have in their fingers.

func viIsWord(r rune) bool { return isWordRune(r) }

func viClass(r rune, big bool) int {
	switch {
	case unicode.IsSpace(r):
		return 0
	case big:
		return 1
	case viIsWord(r):
		return 1
	default:
		return 2
	}
}

// viWordForward is `w`: the start of the next word.
func viWordForward(text []rune, i int, big bool) int {
	n := len(text)
	if i >= n {
		return n
	}
	c := viClass(text[i], big)
	if c != 0 {
		for i < n && viClass(text[i], big) == c {
			i++
		}
	}
	for i < n && viClass(text[i], big) == 0 {
		i++
	}
	return i
}

// viWordBack is `b`: the start of the previous word.
func viWordBack(text []rune, i int, big bool) int {
	if i <= 0 {
		return 0
	}
	i--
	for i > 0 && viClass(text[i], big) == 0 {
		i--
	}
	c := viClass(text[i], big)
	for i > 0 && viClass(text[i-1], big) == c && c != 0 {
		i--
	}
	return i
}

// viWordEnd is `e`: the last character of the current or next word.
func viWordEnd(text []rune, i int, big bool) int {
	n := len(text)
	if i >= n-1 {
		return max(n-1, 0)
	}
	i++
	for i < n && viClass(text[i], big) == 0 {
		i++
	}
	if i >= n {
		return n - 1
	}
	c := viClass(text[i], big)
	for i+1 < n && viClass(text[i+1], big) == c {
		i++
	}
	return i
}

// viWordStartAt is the start of the word the cursor is inside — the left
// edge of `iw`.
func viWordStartAt(text []rune, i int, big bool) int {
	if i >= len(text) || len(text) == 0 {
		return max(min(i, len(text)), 0)
	}
	c := viClass(text[i], big)
	for i > 0 && viClass(text[i-1], big) == c {
		i--
	}
	return i
}
