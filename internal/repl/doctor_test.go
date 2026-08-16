package repl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

// runDoctorScript runs `doctor` through the full RunReader path with
// every state location redirected into temp dirs.
func runDoctorScript(t *testing.T, src string) (stdout string, err error) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("GISH_RC", filepath.Join(base, "gishrc"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("HOME", base)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	var out strings.Builder
	err = RunReader(t.Context(), strings.NewReader(src), "test",
		interp.StdIO(nil, &out, &out))
	return out.String(), err
}

func TestDoctorHealthyExitsZero(t *testing.T) {
	out, err := runDoctorScript(t, "doctor\n")
	if err != nil {
		t.Fatalf("healthy doctor should exit 0: %v\n%s", err, out)
	}
	for _, label := range []string{"rc", "theme", "lint", "history", "plugins", "terminal"} {
		if !strings.Contains(out, label) {
			t.Errorf("missing %q check:\n%s", label, out)
		}
	}
	if strings.Contains(out, "✘") {
		t.Errorf("healthy setup reports a failure:\n%s", out)
	}
	// Nothing exists yet: rc, history, and plugins report clean defaults.
	for _, want := range []string{"no rc file", "not created yet", "none installed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorReportsBrokenRC(t *testing.T) {
	base := t.TempDir()
	rc := filepath.Join(base, "gishrc")
	if err := os.WriteFile(rc, []byte("if broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GISH_RC", rc)
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("HOME", base)
	t.Setenv("TERM", "xterm-256color")
	var out strings.Builder
	err := RunReader(t.Context(), strings.NewReader("doctor\n"), "test",
		interp.StdIO(nil, &out, &out))
	if err == nil {
		t.Errorf("broken rc should exit nonzero:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "does not parse") || !strings.Contains(out.String(), "fix:") {
		t.Errorf("no parse diagnosis with fix:\n%s", out.String())
	}
}

func TestDoctorWarnsOnBadThemeValues(t *testing.T) {
	out, err := runDoctorScript(t,
		"GISH_THEME=p10k GISH_THEME_COLOR_DIR='; rm' GISH_THEME_LINES=9\ndoctor\n")
	if err != nil {
		t.Fatalf("warnings must not fail doctor: %v\n%s", err, out)
	}
	for _, want := range []string{"⚠", "DIR", "GISH_THEME_LINES"} {
		if !strings.Contains(out, want) {
			t.Errorf("theme warning missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorWarnsOnMissingStarship(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no starship (and no shellcheck) here
	out, err := runDoctorScript(t, "GISH_THEME=starship\ndoctor\n")
	if err != nil {
		t.Fatalf("missing starship is a warning, not a failure: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no starship binary") {
		t.Errorf("starship warning missing:\n%s", out)
	}
	if !strings.Contains(out, "shellcheck is not on PATH") {
		t.Errorf("shellcheck warning missing:\n%s", out)
	}
}

func TestDoctorFlagsNonExecutablePlugin(t *testing.T) {
	base := t.TempDir()
	plugins := filepath.Join(base, "data", "gish", "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	// Not a plugin candidate on any platform: no exec bit on unix, no
	// executable extension on Windows.
	if err := os.WriteFile(filepath.Join(plugins, "gish-thing"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GISH_RC", filepath.Join(base, "gishrc"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("HOME", base)
	t.Setenv("TERM", "xterm-256color")
	var out strings.Builder
	if err := RunReader(t.Context(), strings.NewReader("doctor\n"), "test",
		interp.StdIO(nil, &out, &out)); err != nil {
		t.Fatalf("non-executable plugin is a warning, not a failure: %v\n%s", err, out.String())
	}
	fixHint := "chmod +x"
	if runtime.GOOS == "windows" {
		fixHint = "executable extension"
	}
	if !strings.Contains(out.String(), "not executable") || !strings.Contains(out.String(), fixHint) {
		t.Errorf("plugin diagnosis missing:\n%s", out.String())
	}
}

func TestDoctorWarnsOnUnparsableHistoryTail(t *testing.T) {
	base := t.TempDir()
	histDir := filepath.Join(base, "data", "gish")
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"cmd":"ls"}` + "\nnot json{{{\n"
	if err := os.WriteFile(filepath.Join(histDir, "history.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GISH_RC", filepath.Join(base, "gishrc"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("HOME", base)
	t.Setenv("TERM", "xterm-256color")
	var out strings.Builder
	if err := RunReader(t.Context(), strings.NewReader("doctor\n"), "test",
		interp.StdIO(nil, &out, &out)); err != nil {
		t.Fatalf("unparsable tail is a warning, not a failure: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "unparsable tail") {
		t.Errorf("history diagnosis missing:\n%s", out.String())
	}
}
