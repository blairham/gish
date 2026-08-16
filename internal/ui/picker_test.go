package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func items(values ...string) []PickerItem {
	out := make([]PickerItem, len(values))
	for i, v := range values {
		out[i] = PickerItem{Value: v, Detail: "detail-" + v}
	}
	return out
}

// press feeds a key by name, the way bubbletea delivers it.
func press(m pickerModel, keys ...string) pickerModel {
	for _, k := range keys {
		next, _ := m.Update(tea.KeyPressMsg{Code: keyCodeFor(k), Text: textFor(k)})
		m = next.(pickerModel)
	}
	return m
}

func TestPickerNavigationAndSelection(t *testing.T) {
	t.Parallel()

	m := newPickerModel(items("alpha", "beta", "gamma"), PickerOptions{Height: 10})
	if len(m.shown) != 3 {
		t.Fatalf("shown = %d", len(m.shown))
	}
	// Cursor starts at the top; down moves, and does not run off the end.
	m = press(m, "down", "down", "down", "down")
	if got := m.selection(); len(got) != 1 || got[0] != "gamma" {
		t.Errorf("selection after clamping = %v", got)
	}
	m = press(m, "up")
	if got := m.selection(); got[0] != "beta" {
		t.Errorf("up = %v", got)
	}
}

func TestPickerFiltersAsYouType(t *testing.T) {
	t.Parallel()

	m := newPickerModel(items("git status", "go build", "grep here"), PickerOptions{Height: 10})
	m = press(m, "g", "b")
	if len(m.shown) != 1 {
		t.Fatalf("query 'gb' matched %d rows, want 1", len(m.shown))
	}
	if got := m.selection(); got[0] != "go build" {
		t.Errorf("filtered selection = %v", got)
	}
	// Backspace widens the search again.
	m = press(m, "backspace")
	if len(m.shown) < 2 {
		t.Errorf("backspace did not widen: %d rows", len(m.shown))
	}
}

func TestPickerMultiSelect(t *testing.T) {
	t.Parallel()

	m := newPickerModel(items("one", "two", "three"), PickerOptions{Height: 10, Multi: true})
	// Tab marks and advances, so two tabs mark the first two rows.
	m = press(m, "tab", "tab")
	got := m.selection()
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("multi selection = %v", got)
	}
	// Tab on a marked row unmarks it.
	m = press(m, "up", "up", "tab")
	if got = m.selection(); len(got) != 1 || got[0] != "two" {
		t.Errorf("unmark failed: %v", got)
	}
}

func TestPickerAbortSelectsNothing(t *testing.T) {
	t.Parallel()

	m := newPickerModel(items("a", "b"), PickerOptions{Height: 10})
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(pickerModel)
	if m.accepted || cmd == nil {
		t.Errorf("esc should abort and quit: accepted=%v cmd=%v", m.accepted, cmd)
	}
}

func TestPickerViewShowsMetadataAndCounts(t *testing.T) {
	t.Parallel()

	list := items("alpha", "beta")
	list[1].Bad = true
	m := newPickerModel(list, PickerOptions{Height: 10, Prompt: "history"})
	view := m.View().Content
	for _, want := range []string{"history", "detail-alpha", "2/2"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	// An empty result set says so rather than rendering blank.
	m = press(m, "z", "z", "z")
	if !strings.Contains(m.View().Content, "no matches") {
		t.Errorf("empty view = %q", m.View().Content)
	}
}

// keyCodeFor/textFor map the few key names these tests use.
func keyCodeFor(name string) rune {
	switch name {
	case "up":
		return tea.KeyUp
	case "down":
		return tea.KeyDown
	case "tab":
		return tea.KeyTab
	case "backspace":
		return tea.KeyBackspace
	default:
		return []rune(name)[0]
	}
}

func textFor(name string) string {
	switch name {
	case "up", "down", "tab", "backspace":
		return ""
	default:
		return name
	}
}
