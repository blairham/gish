package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/blairham/koi-shell/internal/term"
)

// The selection primitive (#100). fzf's install count is the evidence
// that shells are missing this, not that fzf is unusually good — so
// koi ships one picker and every surface reuses it: ctrl-r history,
// the `pick` builtin, and later completion menus and blocks.
//
// It runs like any full-screen program: the caller hands over the
// terminal, the picker owns it, and the caller takes it back. Piped or
// colorless callers do not get a TUI at all — PickerResult reports
// that so the caller can degrade rather than hang.

// PickerItem is one row: the value returned on selection, plus optional
// columns that make the choice informed rather than a guess.
type PickerItem struct {
	// Value is what selection returns.
	Value string
	// Detail is dimmed metadata rendered after the value (cwd, age,
	// exit status — whatever the surface knows).
	Detail string
	// Bad marks a row that failed, so the eye can skip it.
	Bad bool
}

// PickerOptions configure one invocation.
type PickerOptions struct {
	// Prompt is the label beside the query input.
	Prompt string
	// Multi allows Tab-toggled multi-selection (fzf's habit).
	Multi bool
	// Height is the visible row count; 0 picks a sensible default.
	Height int
	// Query pre-fills the filter — ctrl-r hands over what was typed.
	Query string
}

// Pick runs the picker over items. ok=false means the user aborted;
// selected is empty in that case. When the terminal cannot host a TUI,
// Pick reports usable=false and selects nothing, leaving the fallback
// to the caller.
func Pick(in io.Reader, out io.Writer, items []PickerItem, opts PickerOptions) (selected []string, ok, usable bool) {
	if !pickable(in, out) {
		return nil, false, false
	}
	if opts.Height <= 0 {
		opts.Height = 12
	}
	// A terminal that reports no usable size cannot host a full-screen
	// picker — the renderer would spin on an unsatisfiable layout — so
	// refuse and let the caller degrade (ctrl-r falls back to
	// incremental search, `pick` says why).
	cols, rows, sized := terminalSize(out)
	if !sized {
		return nil, false, false
	}
	if opts.Height > rows-4 {
		opts.Height = max(rows-4, 3)
	}
	model := newPickerModel(items, opts)
	p := tea.NewProgram(model,
		tea.WithOutput(out), tea.WithInput(in), tea.WithoutSignalHandler(),
		tea.WithWindowSize(cols, rows))
	final, err := p.Run()
	if err != nil {
		return nil, false, true
	}
	m, isModel := final.(pickerModel)
	if !isModel || !m.accepted {
		return nil, false, true
	}
	return m.selection(), true, true
}

// terminalSize reports the writer's dimensions. sized=false means the
// terminal will not say, or says something a UI cannot be drawn in.
func terminalSize(out io.Writer) (cols, rows int, sized bool) {
	f, ok := out.(*os.File)
	if !ok {
		return 0, 0, false
	}
	c, r, err := term.NewTTY(f, f).Size()
	if err != nil || c < 20 || r < 6 {
		return 0, 0, false
	}
	return c, r, true
}

// pickable reports whether both ends of the terminal are real.
func pickable(in io.Reader, out io.Writer) bool {
	f, isFile := in.(*os.File)
	return isFile && term.IsTerminal(f) && Enabled(out)
}

type pickerModel struct {
	items  []PickerItem
	opts   PickerOptions
	query  []rune
	shown  []Match
	cursor int
	// marked holds multi-select choices by item index.
	marked   map[int]bool
	accepted bool
	// offset is the first visible row, for scrolling.
	offset int
}

func newPickerModel(items []PickerItem, opts PickerOptions) pickerModel {
	m := pickerModel{
		items:  items,
		opts:   opts,
		query:  []rune(opts.Query),
		marked: map[int]bool{},
	}
	m.refilter()
	return m
}

func (m *pickerModel) refilter() {
	values := make([]string, len(m.items))
	for i, it := range m.items {
		values[i] = it.Value + " " + it.Detail
	}
	m.shown = FuzzyFilter(values, string(m.query))
	m.cursor, m.offset = 0, 0
}

func (m pickerModel) Init() tea.Cmd { return nil }

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "esc", "ctrl+c", "ctrl+g":
		return m, tea.Quit // accepted stays false: abort
	case "enter":
		m.accepted = len(m.shown) > 0 || len(m.marked) > 0
		return m, tea.Quit
	case "up", "ctrl+p", "ctrl+k":
		m.move(-1)
	case "down", "ctrl+n", "ctrl+j":
		m.move(1)
	case "pgup":
		m.move(-m.opts.Height)
	case "pgdown":
		m.move(m.opts.Height)
	case "tab":
		if m.opts.Multi && m.cursor < len(m.shown) {
			idx := m.shown[m.cursor].Index
			if m.marked[idx] {
				delete(m.marked, idx)
			} else {
				m.marked[idx] = true
			}
			m.move(1)
		}
	case "backspace":
		if len(m.query) > 0 {
			m.query = m.query[:len(m.query)-1]
			m.refilter()
		}
	case "ctrl+u":
		m.query = nil
		m.refilter()
	default:
		if r := key.String(); len([]rune(r)) == 1 {
			m.query = append(m.query, []rune(r)...)
			m.refilter()
		}
	}
	return m, nil
}

func (m *pickerModel) move(delta int) {
	if len(m.shown) == 0 {
		return
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.shown)-1)
	switch {
	case m.cursor < m.offset:
		m.offset = m.cursor
	case m.cursor >= m.offset+m.opts.Height:
		m.offset = m.cursor - m.opts.Height + 1
	}
}

// selection returns the marked rows, or the row under the cursor.
func (m pickerModel) selection() []string {
	if len(m.marked) > 0 {
		var out []string
		for i, it := range m.items {
			if m.marked[i] {
				out = append(out, it.Value)
			}
		}
		return out
	}
	if m.cursor < len(m.shown) {
		return []string{m.items[m.shown[m.cursor].Index].Value}
	}
	return nil
}

func (m pickerModel) View() tea.View {
	style := Styles(true)
	var b strings.Builder

	prompt := m.opts.Prompt
	if prompt == "" {
		prompt = "search"
	}
	fmt.Fprintf(&b, "%s %s%s\n", style.Accent.Render(prompt+":"), string(m.query),
		style.Dim.Render("▏"))
	fmt.Fprintf(&b, "%s\n", style.Dim.Render(fmt.Sprintf("%d/%d", len(m.shown), len(m.items))))

	end := min(m.offset+m.opts.Height, len(m.shown))
	for i := m.offset; i < end; i++ {
		item := m.items[m.shown[i].Index]
		marker := "  "
		if m.marked[m.shown[i].Index] {
			marker = style.OK.Render("✓ ")
		}
		value := item.Value
		if item.Bad {
			value = style.Fail.Render(value)
		}
		line := marker + value
		if item.Detail != "" {
			line += "  " + style.Dim.Render(item.Detail)
		}
		if i == m.cursor {
			line = style.Bold.Render("▸ ") + line
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	if len(m.shown) == 0 {
		b.WriteString(style.Dim.Render("  no matches") + "\n")
	}
	return tea.NewView(b.String())
}
