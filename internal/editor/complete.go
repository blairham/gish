package editor

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Candidate is one completion offered to the editor. Value is the raw
// replacement for the word being completed — the editor escapes it on
// insertion (the host owns quoting; a candidate can never inject shell
// metacharacters).
type Candidate struct {
	Value   string
	Display string
}

// CompleteResult is the answer to a completion request: candidates for
// the word starting at rune index WordStart.
type CompleteResult struct {
	WordStart  int
	Candidates []Candidate
}

// maxCandidateList bounds the rendered candidate list.
const maxCandidateList = 60

// completeTab implements Tab: insert the longest common prefix of the
// candidates; a unique candidate completes fully (plus a space, unless
// it's a directory); no progress with several candidates shows the list
// below the edit line until the next keystroke.
func (e *Editor) completeTab() {
	if e.cfg.Complete == nil {
		return
	}
	text, cursor := e.buf.String(), e.buf.Cursor()
	res := e.cfg.Complete(text, cursor)
	if len(res.Candidates) == 0 {
		return
	}
	if res.WordStart < 0 || res.WordStart > cursor {
		return
	}
	word := string([]rune(text)[res.WordStart:cursor])

	if len(res.Candidates) == 1 {
		value := res.Candidates[0].Value
		e.replaceWord(res.WordStart, cursor, value)
		if !strings.HasSuffix(value, "/") {
			e.buf.Insert(" ")
		}
		return
	}

	common := commonPrefix(res.Candidates)
	if len([]rune(common)) > len([]rune(word)) {
		e.replaceWord(res.WordStart, cursor, common)
		return
	}

	// No progress: show what's on offer.
	e.candList = make([]string, 0, min(len(res.Candidates), maxCandidateList))
	for i, c := range res.Candidates {
		if i == maxCandidateList-1 && len(res.Candidates) > maxCandidateList {
			e.candList = append(e.candList, fmt.Sprintf("… (%d more)", len(res.Candidates)-i))
			break
		}
		e.candList = append(e.candList, c.Display)
	}
}

// replaceWord swaps the buffer's [start,cursor) word for the escaped
// replacement value.
func (e *Editor) replaceWord(start, cursor int, value string) {
	e.recordUndo()
	e.buf.deleteRange(start, cursor)
	e.buf.Insert(escapeWord(value))
}

// commonPrefix is the longest shared prefix of the candidate values.
func commonPrefix(cands []Candidate) string {
	prefix := cands[0].Value
	for _, c := range cands[1:] {
		for !strings.HasPrefix(c.Value, prefix) {
			_, size := lastRune(prefix)
			prefix = prefix[:len(prefix)-size]
			if prefix == "" {
				return ""
			}
		}
	}
	return prefix
}

func lastRune(s string) (rune, int) {
	return utf8.DecodeLastRuneInString(s)
}

// escapeWord backslash-escapes shell metacharacters so a completed value
// is always data, never syntax.
func escapeWord(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '\'', '"', '\\', '$', '&', '|', ';', '(', ')', '<', '>', '*', '?', '[', ']', '#', '`', '!', '{', '}':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// candidateRows columnizes the candidate list for the given width.
func candidateRows(items []string, width int) []string {
	if len(items) == 0 {
		return nil
	}
	colW := 0
	for _, it := range items {
		if w := displayWidth(it); w > colW {
			colW = w
		}
	}
	colW += 2
	cols := max(1, width/colW)
	rows := (len(items) + cols - 1) / cols
	out := make([]string, rows)
	for i, it := range items {
		row := i % rows
		if out[row] != "" {
			// Pad the previous cell to the column width.
			pad := colW*(i/rows) - displayWidth(out[row])
			out[row] += strings.Repeat(" ", max(pad, 1))
		}
		out[row] += it
	}
	return out
}
