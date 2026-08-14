package editor

import (
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
)

// Buffer is the editable command text plus a cursor. Text may span
// multiple logical lines ('\n' separated); the cursor is a rune index and
// all movement is grapheme-aware — the cursor never lands inside a
// cluster (emoji, combining marks).
type Buffer struct {
	text   []rune
	cursor int
}

func (b *Buffer) String() string { return string(b.text) }
func (b *Buffer) Cursor() int    { return b.cursor }
func (b *Buffer) Empty() bool    { return len(b.text) == 0 }

// Set replaces the buffer contents, clamping the cursor.
func (b *Buffer) Set(text string, cursor int) {
	b.text = []rune(text)
	b.cursor = min(max(cursor, 0), len(b.text))
}

// Insert places s at the cursor and advances past it.
func (b *Buffer) Insert(s string) {
	if s == "" {
		return
	}
	r := []rune(s)
	b.text = append(b.text[:b.cursor], append(r, b.text[b.cursor:]...)...)
	b.cursor += len(r)
}

// nextBoundary returns the rune index of the grapheme boundary after i.
func nextBoundary(text []rune, i int) int {
	if i >= len(text) {
		return len(text)
	}
	cluster, _, _, _ := uniseg.FirstGraphemeClusterInString(string(text[i:]), -1)
	return i + len([]rune(cluster))
}

// prevBoundary returns the rune index of the grapheme boundary before i.
func prevBoundary(text []rune, i int) int {
	if i <= 0 {
		return 0
	}
	rest := string(text[:i])
	prev, cur, state := 0, 0, -1
	for len(rest) > 0 {
		var cluster string
		cluster, rest, _, state = uniseg.FirstGraphemeClusterInString(rest, state)
		prev = cur
		cur += len([]rune(cluster))
	}
	return prev
}

func (b *Buffer) MoveLeft()  { b.cursor = prevBoundary(b.text, b.cursor) }
func (b *Buffer) MoveRight() { b.cursor = nextBoundary(b.text, b.cursor) }

// DeleteBack removes the grapheme before the cursor.
func (b *Buffer) DeleteBack() {
	start := prevBoundary(b.text, b.cursor)
	b.text = append(b.text[:start], b.text[b.cursor:]...)
	b.cursor = start
}

// DeleteForward removes the grapheme at the cursor.
func (b *Buffer) DeleteForward() {
	end := nextBoundary(b.text, b.cursor)
	b.text = append(b.text[:b.cursor], b.text[end:]...)
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// wordLeft returns the index of the start of the word before i.
func (b *Buffer) wordLeft(i int) int {
	for i > 0 && !isWordRune(b.text[i-1]) {
		i--
	}
	for i > 0 && isWordRune(b.text[i-1]) {
		i--
	}
	return i
}

// wordRight returns the index just past the word after i.
func (b *Buffer) wordRight(i int) int {
	n := len(b.text)
	for i < n && !isWordRune(b.text[i]) {
		i++
	}
	for i < n && isWordRune(b.text[i]) {
		i++
	}
	return i
}

func (b *Buffer) MoveWordLeft()  { b.cursor = b.wordLeft(b.cursor) }
func (b *Buffer) MoveWordRight() { b.cursor = b.wordRight(b.cursor) }

// deleteRange removes [from, to) and returns the removed text.
func (b *Buffer) deleteRange(from, to int) string {
	if from >= to {
		return ""
	}
	killed := string(b.text[from:to])
	b.text = append(b.text[:from], b.text[to:]...)
	if b.cursor >= to {
		b.cursor -= to - from
	} else if b.cursor > from {
		b.cursor = from
	}
	return killed
}

// KillWordBack removes the word before the cursor (Alt-Backspace).
func (b *Buffer) KillWordBack() string {
	return b.deleteRange(b.wordLeft(b.cursor), b.cursor)
}

// KillWordForward removes the word after the cursor (Alt-d).
func (b *Buffer) KillWordForward() string {
	return b.deleteRange(b.cursor, b.wordRight(b.cursor))
}

// KillToWhitespace removes back to the previous whitespace run — the
// unix-word-rubout behavior of Ctrl-W.
func (b *Buffer) KillToWhitespace() string {
	i := b.cursor
	for i > 0 && unicode.IsSpace(b.text[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(b.text[i-1]) {
		i--
	}
	return b.deleteRange(i, b.cursor)
}

// lineStart returns the index just after the previous '\n' (or 0).
func (b *Buffer) lineStart(i int) int {
	for i > 0 && b.text[i-1] != '\n' {
		i--
	}
	return i
}

// lineEnd returns the index of the next '\n' (or len).
func (b *Buffer) lineEnd(i int) int {
	for i < len(b.text) && b.text[i] != '\n' {
		i++
	}
	return i
}

func (b *Buffer) MoveLineStart() { b.cursor = b.lineStart(b.cursor) }
func (b *Buffer) MoveLineEnd()   { b.cursor = b.lineEnd(b.cursor) }
func (b *Buffer) MoveStart()     { b.cursor = 0 }
func (b *Buffer) MoveEnd()       { b.cursor = len(b.text) }

// KillToLineEnd removes from the cursor to end of line; at end of line it
// removes the newline, joining lines (readline Ctrl-K behavior).
func (b *Buffer) KillToLineEnd() string {
	end := b.lineEnd(b.cursor)
	if end == b.cursor && end < len(b.text) {
		end++
	}
	return b.deleteRange(b.cursor, end)
}

// KillToLineStart removes from start of line to the cursor (Ctrl-U).
func (b *Buffer) KillToLineStart() string {
	return b.deleteRange(b.lineStart(b.cursor), b.cursor)
}

// Lines splits the buffer into logical lines.
func (b *Buffer) Lines() []string {
	return strings.Split(string(b.text), "\n")
}

// CursorLine returns the cursor's logical line index and the text of that
// line before the cursor (for width-based positioning).
func (b *Buffer) CursorLine() (line int, before string) {
	start := b.lineStart(b.cursor)
	for _, r := range b.text[:start] {
		if r == '\n' {
			line++
		}
	}
	return line, string(b.text[start:b.cursor])
}

// MoveUp moves the cursor to the previous logical line, preserving the
// rune column where possible. Reports whether it moved.
func (b *Buffer) MoveUp() bool {
	start := b.lineStart(b.cursor)
	if start == 0 {
		return false
	}
	col := b.cursor - start
	prevStart := b.lineStart(start - 1)
	prevLen := (start - 1) - prevStart
	b.cursor = prevStart + min(col, prevLen)
	return true
}

// MoveDown moves the cursor to the next logical line, preserving the rune
// column where possible. Reports whether it moved.
func (b *Buffer) MoveDown() bool {
	end := b.lineEnd(b.cursor)
	if end >= len(b.text) {
		return false
	}
	col := b.cursor - b.lineStart(b.cursor)
	nextStart := end + 1
	nextLen := b.lineEnd(nextStart) - nextStart
	b.cursor = nextStart + min(col, nextLen)
	return true
}
