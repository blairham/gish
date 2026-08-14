package editor

import (
	"fmt"
	"io"
	"strings"

	"github.com/rivo/uniseg"
)

// renderer draws the prompt+buffer region inline (no alt screen) and
// diffs against what is already on screen: only changed rows are
// rewritten. It tracks the cursor's screen row relative to the top of the
// region, so every frame starts by homing to the region origin.
//
// This render pipeline is deliberately gish-owned (#1): plugin ghost
// text, completion menus, and deadline-driven prompt repaints all land
// here later.
type renderer struct {
	out   io.Writer
	width int

	rows   []string // screen rows as last drawn
	curRow int      // cursor's screen row, relative to region top
}

func newRenderer(out io.Writer, width int) *renderer {
	return &renderer{out: out, width: max(width, 1)}
}

func (r *renderer) setWidth(w int) {
	if w >= 1 && w != r.width {
		r.width = w
		// Width changed: previous wrapping is meaningless. Repaint all.
		r.rows = nil
	}
}

// wrapLine splits one logical line into screen rows by display width,
// breaking on grapheme boundaries.
func wrapLine(line string, width int) []string {
	if line == "" {
		return []string{""}
	}
	var (
		rows  []string
		row   strings.Builder
		rowW  int
		state = -1
		rest  = line
	)
	for len(rest) > 0 {
		var cluster string
		var w int
		cluster, rest, w, state = uniseg.FirstGraphemeClusterInString(rest, state)
		if rowW+w > width && rowW > 0 {
			rows = append(rows, row.String())
			row.Reset()
			rowW = 0
		}
		row.WriteString(cluster)
		rowW += w
	}
	rows = append(rows, row.String())
	return rows
}

// frame lays out prompt-prefixed logical lines into screen rows and the
// cursor's screen position. cursorLine indexes lines; cursorW is the
// display width of everything left of the cursor on that line (prompt
// included).
func frame(lines []string, width, cursorLine, cursorW int) (rows []string, curRow, curCol int) {
	for i, ln := range lines {
		if i == cursorLine {
			curRow = len(rows) + cursorW/width
			curCol = cursorW % width
		}
		rows = append(rows, wrapLine(ln, width)...)
	}
	return rows, curRow, curCol
}

// render draws the region. lines are logical lines with prompts already
// prefixed; cursorLine/cursorW as in frame.
func (r *renderer) render(lines []string, cursorLine, cursorW int) {
	rows, curRow, curCol := frame(lines, r.width, cursorLine, cursorW)

	var b strings.Builder
	// Home to the region origin.
	if r.curRow > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", r.curRow)
	}
	b.WriteString("\r")

	for i, row := range rows {
		if i > 0 {
			b.WriteString("\r\n")
		}
		if i < len(r.rows) && r.rows[i] == row {
			continue // unchanged row: just moved past it
		}
		b.WriteString("\x1b[2K") // clear the row, then repaint it
		b.WriteString(row)
		b.WriteString("\r")
	}

	// Clear any leftover rows from a taller previous frame, then return.
	if extra := len(r.rows) - len(rows); extra > 0 {
		b.WriteString("\x1b[1B\r\x1b[J\x1b[1A\r")
	}

	// Position the cursor: currently at start of the last row.
	if up := len(rows) - 1 - curRow; up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	if curCol > 0 {
		fmt.Fprintf(&b, "\x1b[%dC", curCol)
	}

	io.WriteString(r.out, b.String()) //nolint:errcheck // terminal write; nowhere to report

	r.rows = rows
	r.curRow = curRow
}

// finish moves the cursor below the region and resets state — called
// after a command is accepted or canceled so shell output starts on a
// fresh line.
func (r *renderer) finish() {
	if down := len(r.rows) - 1 - r.curRow; down > 0 {
		fmt.Fprintf(r.out, "\x1b[%dB", down)
	}
	io.WriteString(r.out, "\r\n") //nolint:errcheck // terminal write
	r.rows = nil
	r.curRow = 0
}

// clearScreen erases the display and repaints from the top (Ctrl-L).
func (r *renderer) clearScreen() {
	io.WriteString(r.out, "\x1b[2J\x1b[H") //nolint:errcheck // terminal write
	r.rows = nil
	r.curRow = 0
}

// displayWidth is the on-screen width of s.
func displayWidth(s string) int {
	return uniseg.StringWidth(s)
}
