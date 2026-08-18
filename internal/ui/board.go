package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/blairham/koi-shell/internal/term"
)

// The live task board (#90): rows appear queued, spin while running,
// and settle into ✓/✗ with their outcome — the zi update fan-out's
// display. Generic on purpose: anything emitting BoardEvents (ordered
// indexes, started/done transitions) can render through it.
//
// This runs between reads like any command owning the terminal — the
// editor's render loop is not involved (decision #1 stands).

// BoardEvent is one row transition.
type BoardEvent struct {
	Index   int
	Name    string
	Started bool // a worker picked it up
	Done    bool
	Outcome string
	Failed  bool
}

// RunBoard renders the board on w while run executes. run receives a
// goroutine-safe emit and its error is returned. The live region shows
// every row; on exit tea clears its UI and RunBoard prints the settled
// summary once, in index order — the same lines the plain path would
// have produced, deterministically (a Println-per-row would race the
// quit on fast engines).
//
// The board owns the terminal like any TUI while it runs: in reads the
// terminal so tea's capability-probe replies are consumed instead of
// leaking into the shell's next editor read. Type-ahead typed during a
// board is consumed by it — the standard full-screen-program contract,
// traded knowingly against #1's between-command preservation.
func RunBoard(w io.Writer, in io.Reader, run func(emit func(BoardEvent)) error) error {
	model := newBoardModel()
	var p *tea.Program
	// The engine starts from Init, once the program is processing
	// messages — events sent before Run begins would be dropped.
	model.start = func() tea.Msg {
		return boardDone{err: run(func(ev BoardEvent) { p.Send(ev) })}
	}
	opts := []tea.ProgramOption{
		tea.WithOutput(w),
		tea.WithInput(in),
		tea.WithoutSignalHandler(),
	}
	if f, ok := w.(*os.File); !ok || !term.IsTerminal(f) {
		// No terminal, no WindowSizeMsg: give the renderer a size or it
		// clips every frame to zero width (tests, forced-board runs).
		opts = append(opts, tea.WithWindowSize(80, 24))
	}
	p = tea.NewProgram(model, opts...)
	final, perr := p.Run()
	if perr != nil {
		return perr
	}
	m, ok := final.(boardModel)
	if !ok {
		return nil
	}
	for _, row := range m.rows {
		fmt.Fprintln(w, renderRow(row, 0))
	}
	return m.err
}

type boardRow struct {
	name    string
	outcome string
	started bool
	done    bool
	failed  bool
}

type boardModel struct {
	rows  []boardRow
	frame int
	err   error

	start tea.Cmd // runs the engine; wired by RunBoard before Run
}

type boardDone struct{ err error }

type boardTick struct{}

func newBoardModel() boardModel { return boardModel{} }

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return boardTick{} })
}

func (m boardModel) Init() tea.Cmd { return tea.Batch(tick(), m.start) }

func (m boardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case BoardEvent:
		for len(m.rows) <= msg.Index {
			m.rows = append(m.rows, boardRow{})
		}
		row := &m.rows[msg.Index]
		if msg.Name != "" {
			row.name = msg.Name
		}
		row.started = row.started || msg.Started || msg.Done
		if msg.Done {
			row.done, row.outcome, row.failed = true, msg.Outcome, msg.Failed
		}
		return m, nil
	case boardTick:
		m.frame++
		return m, tick()
	case boardDone:
		m.err = msg.err
		return m, tea.Quit
	}
	return m, nil
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// renderRow renders one row; frame drives the spinner.
func renderRow(row boardRow, frame int) string {
	style := Styles(true)
	var marker, outcome string
	switch {
	case row.done && row.failed:
		marker = style.Fail.Render("✗")
		outcome = style.Fail.Render(row.outcome)
	case row.done:
		marker = style.OK.Render("✓")
		outcome = style.Dim.Render(row.outcome)
	case row.started:
		marker = style.Accent.Render(spinnerFrames[frame%len(spinnerFrames)])
	default:
		marker = style.Dim.Render("·")
	}
	return strings.TrimRight(fmt.Sprintf("%s %-40s %s", marker, row.name, outcome), " ")
}

// View is the live region: every row, settled ones included; the
// final summary print replaces it on exit.
func (m boardModel) View() tea.View {
	var b strings.Builder
	for _, row := range m.rows {
		b.WriteString(renderRow(row, m.frame) + "\n")
	}
	return tea.NewView(b.String())
}
