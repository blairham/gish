package editor

const undoMax = 200

type bufState struct {
	text   string
	cursor int
}

// undoStack holds buffer snapshots. Runs of self-insertion coalesce into
// one entry (the editor decides when to record), so undo steps back by
// edit, not by keystroke.
type undoStack struct {
	states []bufState
}

func (u *undoStack) push(b *Buffer) {
	u.states = append(u.states, bufState{text: b.String(), cursor: b.Cursor()})
	if len(u.states) > undoMax {
		u.states = u.states[1:]
	}
}

func (u *undoStack) pop() (bufState, bool) {
	if len(u.states) == 0 {
		return bufState{}, false
	}
	s := u.states[len(u.states)-1]
	u.states = u.states[:len(u.states)-1]
	return s, true
}

// bottom is the oldest snapshot: the line as it was before this round of
// editing began. revert-line (Alt-r) wants that one, not the previous
// one — "undo everything I did to this line" is a different request from
// "undo my last change".
func (u *undoStack) bottom() (bufState, bool) {
	if len(u.states) == 0 {
		return bufState{}, false
	}
	return u.states[0], true
}

func (u *undoStack) reset() { u.states = u.states[:0] }
