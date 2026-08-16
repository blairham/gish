package repl

import (
	"strings"
	"testing"
)

func TestMarkPromptWrapsWithoutDisturbingContent(t *testing.T) {
	t.Parallel()

	got := markPrompt("gish$ ", true)
	if !strings.HasPrefix(got, "\x1b]133;A") || !strings.HasSuffix(got, "\x1b]133;B\x1b\\") {
		t.Errorf("marks missing: %q", got)
	}
	if !strings.Contains(got, "gish$ ") {
		t.Errorf("prompt content lost: %q", got)
	}
	// Off means byte-identical: a terminal that hates OSC gets nothing.
	if got := markPrompt("gish$ ", false); got != "gish$ " {
		t.Errorf("disabled marks still wrote: %q", got)
	}
}

func TestMarkOutputAndDone(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	markOutputStart(&b, true)
	markCommandDone(&b, true, 3)
	got := b.String()
	if !strings.Contains(got, "\x1b]133;C") {
		t.Errorf("no output mark: %q", got)
	}
	if !strings.Contains(got, "\x1b]133;D;3") {
		t.Errorf("exit status missing from the done mark: %q", got)
	}

	b.Reset()
	markOutputStart(&b, false)
	markCommandDone(&b, false, 0)
	if b.Len() != 0 {
		t.Errorf("disabled marks wrote %q", b.String())
	}
}

func TestSemanticMarksOptOut(t *testing.T) {
	runner := newTestRunner(t)
	if !semanticMarksOn(runner) {
		t.Error("marks should default on: they are inert where unsupported")
	}
	if err := runner.Run(t.Context(), parseLine(t, `GISH_SEMANTIC_MARKS=off`)); err != nil {
		t.Fatal(err)
	}
	if semanticMarksOn(runner) {
		t.Error("opt-out ignored")
	}
}

func TestDoctorNamesKnownTerminals(t *testing.T) {
	t.Setenv("KITTY_WINDOW_ID", "1")
	detail, known := doctorSemanticMarks()
	if !known || !strings.Contains(detail, "kitty") {
		t.Errorf("kitty not recognized: %q %v", detail, known)
	}

	t.Setenv("KITTY_WINDOW_ID", "")
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("WEZTERM_PANE", "")
	t.Setenv("GHOSTTY_RESOURCES_DIR", "")
	detail, known = doctorSemanticMarks()
	if known {
		t.Errorf("unknown terminal claimed support: %q", detail)
	}
	// An unknown terminal is reported honestly, not as a failure.
	if !strings.Contains(detail, "may or may not") {
		t.Errorf("unknown terminal detail = %q", detail)
	}
}
