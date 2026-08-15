package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

// runConfigScript runs src through the full RunReader path (which stacks
// configCallHandler) with the rc file redirected to a temp path.
func runConfigScript(t *testing.T, rc, src string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("GISH_RC", rc)
	var out, errOut strings.Builder
	err = RunReader(t.Context(), strings.NewReader(src), "test",
		interp.StdIO(nil, &out, &errOut))
	return out.String(), errOut.String(), err
}

func TestConfigSetPersistsAndGoesLive(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "gishrc")
	out, _, err := runConfigScript(t, rc, "config theme starship\necho live=$GISH_THEME\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=starship") {
		t.Errorf("setting not live in the session: %q", out)
	}
	if !strings.Contains(out, "saved to") {
		t.Errorf("no confirmation: %q", out)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "GISH_THEME=starship\n" {
		t.Errorf("rc = %q", got)
	}
}

func TestConfigRewritesExistingAssignment(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "gishrc")
	orig := "# my rc\nexport GISH_THEME=p10k\nalias ll='ls -l'\n"
	if err := os.WriteFile(rc, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runConfigScript(t, rc, "config theme plain\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	want := "# my rc\nexport GISH_THEME=plain\nalias ll='ls -l'\n"
	if string(data) != want {
		t.Errorf("rc = %q, want %q", data, want)
	}
}

func TestConfigQuotesValues(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "gishrc")
	out, _, err := runConfigScript(t, rc, "config prompt '%W $ '\necho live=[$GISH_PROMPT]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=[%W $ ]") {
		t.Errorf("prompt not live: %q", out)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "GISH_PROMPT='%W $ '\n" {
		t.Errorf("rc = %q", got)
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "gishrc")
	_, errOut, _ := runConfigScript(t, rc, "config theme rainbow\n")
	if !strings.Contains(errOut, "must be one of") {
		t.Errorf("stderr = %q", errOut)
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Error("rc file written despite invalid value")
	}

	_, errOut, _ = runConfigScript(t, rc, "config wibble on\n")
	if !strings.Contains(errOut, "unknown setting") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestConfigShow(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "gishrc")
	out, _, err := runConfigScript(t, rc, "config theme p10k\nconfig theme\nconfig\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `theme = "p10k" (GISH_THEME)`) {
		t.Errorf("single show missing: %q", out)
	}
	if !strings.Contains(out, "lint") || !strings.Contains(out, "prompt") {
		t.Errorf("listing missing settings: %q", out)
	}
}

func TestConfigCreatesXDGRCWhenNoneExists(t *testing.T) {
	base := t.TempDir()
	t.Setenv("GISH_RC", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("HOME", base)

	var out strings.Builder
	err := RunReader(t.Context(), strings.NewReader("config lint native\n"), "test",
		interp.StdIO(nil, &out, &out))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(base, "config", "gish", "gishrc"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "GISH_LINT=native\n" {
		t.Errorf("rc = %q", got)
	}
}
