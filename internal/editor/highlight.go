package editor

import "strings"

// HighlightSpan colors the half-open rune range [Start, End) of the
// buffer with an ANSI style. Spans come from the shell's parser-driven
// highlighter; the editor only applies them.
type HighlightSpan struct {
	Start int
	End   int
	Style string
}

const styleReset = "\x1b[0m"

// applyHighlight renders the raw buffer lines with spans injected.
// Span offsets index the whole buffer text (newlines included); spans
// crossing a line boundary are clamped per line so prompts and
// continuation prefixes stay unstyled.
func applyHighlight(raw []string, spans []HighlightSpan) []string {
	if len(spans) == 0 {
		return raw
	}
	out := make([]string, len(raw))
	lineStart := 0
	for i, line := range raw {
		runes := []rune(line)
		lineEnd := lineStart + len(runes)
		var b strings.Builder
		pos := lineStart
		for _, sp := range spans {
			start, end := max(sp.Start, lineStart), min(sp.End, lineEnd)
			if start >= end || start < pos {
				continue // outside this line, or overlapping a prior span
			}
			b.WriteString(string(runes[pos-lineStart : start-lineStart]))
			b.WriteString(sp.Style)
			b.WriteString(string(runes[start-lineStart : end-lineStart]))
			b.WriteString(styleReset)
			pos = end
		}
		b.WriteString(string(runes[pos-lineStart:]))
		out[i] = b.String()
		lineStart = lineEnd + 1 // the '\n'
	}
	return out
}

// ghostText returns the suggestion remainder to render after the cursor:
// only for single-line buffers with the cursor at the end, and only when
// the suggestion strictly extends the buffer.
func (e *Editor) ghostText() string {
	if e.cfg.Suggest == nil || e.search.active {
		return ""
	}
	text := e.buf.String()
	if text == "" || e.buf.Cursor() != len([]rune(text)) || strings.ContainsRune(text, '\n') {
		return ""
	}
	s := e.cfg.Suggest(text)
	if s == "" || !strings.HasPrefix(s, text) || len(s) <= len(text) {
		return ""
	}
	return s[len(text):]
}

// acceptGhost inserts the current suggestion remainder; reports whether
// there was one.
func (e *Editor) acceptGhost() bool {
	ghost := e.ghostText()
	if ghost == "" {
		return false
	}
	e.recordUndo()
	e.buf.Insert(ghost)
	return true
}

// moveRightOrAccept is the Right/Ctrl-F behavior with suggestions: at
// the end of the buffer it accepts the ghost text, otherwise it moves.
func (e *Editor) moveRightOrAccept() {
	if e.buf.Cursor() == len([]rune(e.buf.String())) && e.acceptGhost() {
		return
	}
	e.buf.MoveRight()
}

// moveLineEndOrAccept: End accepts the ghost when already at the end.
func (e *Editor) moveLineEndOrAccept() {
	if e.buf.Cursor() == len([]rune(e.buf.String())) && e.acceptGhost() {
		return
	}
	e.buf.MoveLineEnd()
}
