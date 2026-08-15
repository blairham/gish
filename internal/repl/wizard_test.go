package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// wizardHC builds a HandlerContext with scripted answers on stdin and
// the rc file redirected to a temp path.
func wizardHC(t *testing.T, answers string, env []string, out *strings.Builder) (interp.HandlerContext, string) {
	t.Helper()
	rc := filepath.Join(t.TempDir(), "gishrc")
	t.Setenv("GISH_RC", rc)
	return interp.HandlerContext{
		Env:    expand.ListEnviron(env...),
		Stdin:  strings.NewReader(answers),
		Stdout: out,
		Stderr: out,
	}, rc
}

func TestWizardFullWalkthrough(t *testing.T) {
	var out strings.Builder
	// lines=1 skips the frame question (a one-line prompt has no frame).
	hc, rc := wizardHC(t, "p10k\npowerline\n1\ndir git exit\ny\n", nil, &out)

	got := runThemeWizard(hc)
	if len(got) != 2 || got[0] != "eval" {
		t.Fatalf("wizard result = %v, want an eval", got)
	}
	for _, assign := range []string{
		"GISH_THEME=p10k", "GISH_THEME_SEP=powerline",
		"GISH_THEME_LINES=1", "GISH_THEME_SEGMENTS='dir git exit'",
	} {
		if !strings.Contains(got[1], assign) {
			t.Errorf("live assignments missing %q: %q", assign, got[1])
		}
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"GISH_THEME=p10k\n", "GISH_THEME_SEP=powerline\n",
		"GISH_THEME_LINES=1\n", "GISH_THEME_SEGMENTS='dir git exit'\n",
	} {
		if !strings.Contains(string(data), line) {
			t.Errorf("rc missing %q:\n%s", line, data)
		}
	}
	// The walkthrough previews the chosen layout before asking to save.
	if !strings.Contains(out.String(), "preview") || !strings.Contains(out.String(), "❯") {
		t.Errorf("no rendered preview:\n%s", out.String())
	}
}

func TestWizardEnterKeepsCurrent(t *testing.T) {
	var out strings.Builder
	env := []string{
		"GISH_THEME=p10k", "GISH_THEME_SEP=plain", "GISH_THEME_LINES=2",
		"GISH_THEME_FRAME=on", "GISH_THEME_SEGMENTS=dir git",
	}
	hc, rc := wizardHC(t, "\n\n\n\n\n\n", env, &out)

	got := runThemeWizard(hc)
	if len(got) != 1 || got[0] != "true" {
		t.Fatalf("all-Enter wizard = %v, want true", got)
	}
	if !strings.Contains(out.String(), "nothing changed") {
		t.Errorf("expected nothing-changed notice:\n%s", out.String())
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Error("rc file written when nothing changed")
	}
}

func TestWizardDecliningSavesNothing(t *testing.T) {
	var out strings.Builder
	hc, rc := wizardHC(t, "p10k\nplain\n2\non\ndir\nn\n", nil, &out)

	got := runThemeWizard(hc)
	if len(got) != 1 || got[0] != "true" {
		t.Fatalf("declined wizard = %v, want true", got)
	}
	if !strings.Contains(out.String(), "nothing saved") {
		t.Errorf("expected nothing-saved notice:\n%s", out.String())
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Error("rc file written despite declining")
	}
}

func TestWizardEOFAborts(t *testing.T) {
	var out strings.Builder
	hc, rc := wizardHC(t, "", nil, &out)

	got := runThemeWizard(hc)
	if len(got) != 1 || got[0] != "true" {
		t.Fatalf("EOF wizard = %v, want true", got)
	}
	if !strings.Contains(out.String(), "nothing saved") {
		t.Errorf("expected nothing-saved notice:\n%s", out.String())
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Error("rc file written despite EOF abort")
	}
}

func TestWizardReasksOnInvalidAnswers(t *testing.T) {
	var out strings.Builder
	// Bad theme, then a valid run with a bad segment list first.
	hc, _ := wizardHC(t, "rainbow\np10k\nplain\n2\non\ndir;rm\ndir\ny\n", nil, &out)

	got := runThemeWizard(hc)
	if len(got) != 2 || got[0] != "eval" {
		t.Fatalf("wizard result = %v, want an eval", got)
	}
	if !strings.Contains(out.String(), "pick one of") {
		t.Errorf("invalid theme not re-asked:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "bad segment id") {
		t.Errorf("invalid segment list not re-asked:\n%s", out.String())
	}
}

func TestConfigThemeShowsWhenStdinNotTTY(t *testing.T) {
	// Through the full RunReader path stdin is not a terminal, so
	// `config theme` stays the plain show — scripts never hang.
	rc := filepath.Join(t.TempDir(), "gishrc")
	out, _, err := runConfigScript(t, rc, "config theme\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `theme = "" (GISH_THEME)`) {
		t.Errorf("piped `config theme` should show, not launch the wizard: %q", out)
	}
}
