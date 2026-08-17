package repl

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
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

// OSC 7 (#165): the report every modern terminal reads to open a new
// tab or split where the user is.
func TestMarkCwdEncodesThePath(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	markCwd(&b, true, "/tmp/a dir/with#hash")
	got := b.String()
	if !strings.HasPrefix(got, "\x1b]7;file://") {
		t.Fatalf("not an OSC 7 report: %q", got)
	}
	// A directory named with a space or a '#' is ordinary, and an
	// unencoded one produces a URL the terminal parses into somewhere
	// else entirely.
	if !strings.Contains(got, "/tmp/a%20dir/with%23hash") {
		t.Errorf("path not percent-encoded: %q", got)
	}
	if strings.Contains(got, " ") {
		t.Errorf("a raw space survived into the URL: %q", got)
	}

	var off strings.Builder
	markCwd(&off, false, "/tmp")
	if off.String() != "" {
		t.Errorf("disabled OSC 7 still wrote: %q", off.String())
	}
}

// SetUserVar hands the command line to the terminal, which may put it
// in a status bar or a title. The #10 rules apply there for the same
// reason they apply to history.
func TestUserVarsRespectTheSecretRules(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	markUserVars(&b, true, "export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY", 3*time.Second)
	got := b.String()
	if strings.Contains(got, "wJalrXUtnFEMIK") {
		t.Errorf("a secret was published to the terminal: %q", got)
	}
	decoded := decodeUserVar(t, got, "gish_command")
	if decoded != "export" {
		t.Errorf("published command = %q, want just the first word", decoded)
	}

	b.Reset()
	markUserVars(&b, true, "make build", 1500*time.Millisecond)
	if got := decodeUserVar(t, b.String(), "gish_command"); got != "make build" {
		t.Errorf("ordinary command = %q", got)
	}
	if got := decodeUserVar(t, b.String(), "gish_duration_ms"); got != "1500" {
		t.Errorf("duration = %q, want 1500", got)
	}
}

func decodeUserVar(t *testing.T, out, name string) string {
	t.Helper()
	for _, part := range strings.Split(out, "\x1b]1337;SetUserVar=") {
		key, rest, ok := strings.Cut(part, "=")
		if !ok || key != name {
			continue
		}
		value, _, _ := strings.Cut(rest, "\x1b")
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			t.Fatalf("user var %s is not base64: %q", name, value)
		}
		return string(decoded)
	}
	t.Fatalf("no user var %q in %q", name, out)
	return ""
}

// The knob is per-feature, because the three things gish emits carry
// different risks: marks are inert, OSC 7 is a path, and SetUserVar is
// the command line.
func TestSemanticFeatureSelection(t *testing.T) {
	t.Parallel()

	tests := map[string]termFeatures{
		"":             {marks: true, cwd: true},
		"on":           {marks: true, cwd: true},
		"off":          {},
		"marks":        {marks: true},
		"cwd,uservars": {cwd: true, userVars: true},
		"MARKS, CWD":   {marks: true, cwd: true},
		"nonsense":     {},
	}
	for setting, want := range tests {
		got := semanticFeatures(runnerWithVars(t, map[string]string{"GISH_SEMANTIC_MARKS": setting}))
		if got != want {
			t.Errorf("GISH_SEMANTIC_MARKS=%q gave %+v, want %+v", setting, got, want)
		}
	}
}

// The A mark declares click_events, which is the whole implementation
// of click-to-move-cursor: the terminal then sends arrow keys.
func TestPromptMarkDeclaresClickEvents(t *testing.T) {
	t.Parallel()

	if got := markPrompt("$ ", true); !strings.Contains(got, "click_events=1") {
		t.Errorf("click_events not declared: %q", got)
	}
}
