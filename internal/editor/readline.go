package editor

import "strings"

// The muscle-memory set (#96): the readline commands whose absence
// makes twenty-year bash/zsh hands bounce off a new shell. Documented
// abandonment triggers, implemented to readline's semantics.

// yankLastArg is Alt-. / Alt-_: insert the last word of the previous
// history entry; repeating cycles through progressively older entries,
// replacing the previous insertion — readline's yank-last-arg.
func (e *Editor) yankLastArg() {
	if e.hist() == nil {
		return
	}
	n := 0
	if e.lastWasYankArg {
		n = e.yankArgN + 1
	}
	entry, ok := e.hist().Match("", n)
	if !ok {
		if !e.lastWasYankArg {
			return
		}
		// Past the oldest entry: wrap back to the newest, like readline.
		n = 0
		if entry, ok = e.hist().Match("", n); !ok {
			return
		}
	}
	arg := lastArgOf(entry)
	if arg == "" {
		return
	}
	e.recordUndo()
	if e.lastWasYankArg {
		e.buf.Set(e.buf.String()[:e.yankArgStart]+e.buf.String()[e.yankArgEnd:], e.yankArgStart)
	}
	e.yankArgStart = e.buf.Cursor()
	e.buf.Insert(arg)
	e.yankArgEnd = e.buf.Cursor()
	e.yankArgN = n
	e.thisYankArg = true
}

// lastArgOf is the final whitespace-delimited word of a command line.
func lastArgOf(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// commentAccept is Alt-#: comment the line out and accept it —
// readline's insert-comment. The line lands in history un-run, which
// is the point: park a half-typed command without losing it.
func (e *Editor) commentAccept() {
	e.recordUndo()
	e.buf.Set("#"+e.buf.String(), 0)
	e.state = stateAccepted
}

// transposeChars is Ctrl-T: swap the two characters around the cursor
// and advance, readline-style (at line end, swap the final two).
func (e *Editor) transposeChars() {
	text := []rune(e.buf.String())
	cur := e.buf.Cursor()
	if len(text) < 2 {
		return
	}
	if cur >= len(text) {
		cur = len(text) - 1
	}
	if cur == 0 {
		cur = 1
	}
	e.recordUndo()
	text[cur-1], text[cur] = text[cur], text[cur-1]
	e.buf.Set(string(text), min(cur+1, len(text)))
}

// operateAndGetNext is Ctrl-O: accept the current history entry and
// queue the chronologically next one into the following prompt — the
// replay-a-sequence workflow.
func (e *Editor) operateAndGetNext() {
	if e.histPos > 0 {
		if next, ok := e.hist().Match(e.histPrefix, e.histPos-1); ok {
			e.preload = next
		}
	}
	e.acceptOrNewline()
}

// startCtrlX arms the Ctrl-X chord; dispatchKey resolves the second
// key (Ctrl-E → edit-and-execute-command).
func (e *Editor) startCtrlX() {
	e.pendingCtrlX = true
}

// externalEditRequest flags ReadCommand to cede the terminal to
// $EDITOR (Ctrl-X Ctrl-E). The actual edit happens outside the event
// loop, where raw mode and the input decoder can be safely suspended.
func (e *Editor) externalEditRequest() {
	if e.cfg.ExternalEdit != nil {
		e.state = stateExternalEdit
	}
}
