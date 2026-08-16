package ui

import (
	"errors"
	"strings"
	"testing"
)

func TestBoardModelLifecycle(t *testing.T) {
	t.Parallel()

	m := newBoardModel()
	step := func(msg any) {
		next, _ := m.Update(msg)
		m = next.(boardModel)
	}

	// Queued rows render as dots, in index order.
	step(BoardEvent{Index: 0, Name: "alpha"})
	step(BoardEvent{Index: 1, Name: "beta"})
	view := m.View().Content
	if !strings.Contains(view, "·") || !strings.Contains(view, "alpha") {
		t.Errorf("queued view = %q", view)
	}

	// A started row spins.
	step(BoardEvent{Index: 1, Started: true})
	if view = m.View().Content; !strings.Contains(view, spinnerFrames[0]) {
		t.Errorf("started view missing spinner: %q", view)
	}

	// Done rows settle into their outcome, and stay in the live view.
	step(BoardEvent{Index: 1, Done: true, Outcome: "updated abc123"})
	step(BoardEvent{Index: 0, Done: true, Outcome: "boom", Failed: true})
	view = m.View().Content
	if !strings.Contains(view, "✓") || !strings.Contains(view, "updated abc123") {
		t.Errorf("done view = %q", view)
	}
	if !strings.Contains(view, "✗") || !strings.Contains(view, "boom") {
		t.Errorf("failed view = %q", view)
	}

	// Completion carries the run's error and quits.
	next, cmd := m.Update(boardDone{err: errors.New("run failed")})
	m = next.(boardModel)
	if m.err == nil || cmd == nil {
		t.Errorf("boardDone: err=%v cmd=%v", m.err, cmd)
	}
}

func TestRunBoardReturnsRunError(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	err := RunBoard(&out, strings.NewReader(""), func(emit func(BoardEvent)) error {
		emit(BoardEvent{Index: 0, Name: "only"})
		emit(BoardEvent{Index: 0, Done: true, Outcome: "fine"})
		return errors.New("engine error")
	})
	if err == nil || err.Error() != "engine error" {
		t.Errorf("RunBoard err = %v", err)
	}
	if !strings.Contains(out.String(), "only") {
		t.Errorf("settled row never persisted: %q", out.String())
	}
}
