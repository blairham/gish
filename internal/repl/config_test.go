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
	t.Setenv("KOI_RC", rc)
	var out, errOut strings.Builder
	err = RunReader(t.Context(), strings.NewReader(src), "test",
		interp.StdIO(nil, &out, &errOut))
	return out.String(), errOut.String(), err
}

func TestConfigSetPersistsAndGoesLive(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc, "config theme starship\necho live=$KOI_THEME\n")
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
	if got := string(data); got != "KOI_THEME=starship\n" {
		t.Errorf("rc = %q", got)
	}
}

func TestConfigRewritesExistingAssignment(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	orig := "# my rc\nexport KOI_THEME=p10k\nalias ll='ls -l'\n"
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
	want := "# my rc\nexport KOI_THEME=plain\nalias ll='ls -l'\n"
	if string(data) != want {
		t.Errorf("rc = %q, want %q", data, want)
	}
}

func TestConfigQuotesValues(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc, "config prompt '%W $ '\necho live=[$KOI_PROMPT]\n")
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
	if got := string(data); got != "KOI_PROMPT='%W $ '\n" {
		t.Errorf("rc = %q", got)
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
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
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc, "config theme p10k\nconfig theme\nconfig\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `theme = "p10k" (KOI_THEME)`) {
		t.Errorf("single show missing: %q", out)
	}
	if !strings.Contains(out, "lint") || !strings.Contains(out, "prompt") {
		t.Errorf("listing missing settings: %q", out)
	}
}

func TestConfigThemeSegmentToggle(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc,
		"config theme.git off\necho off=[$KOI_THEME_SEGMENTS]\n"+
			"config theme.git on\necho on=[$KOI_THEME_SEGMENTS]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "off=[dir pins jobs duration exit]") {
		t.Errorf("off toggle not live: %q", out)
	}
	// Re-adding a built-in restores its default-order slot.
	if !strings.Contains(out, "on=[dir git pins jobs duration exit]") {
		t.Errorf("on toggle not live in default order: %q", out)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "KOI_THEME_SEGMENTS='dir git pins jobs duration exit'\n" {
		t.Errorf("rc = %q", got)
	}
}

func TestConfigThemePluginSegmentAppends(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc, "config theme.k8s on\necho live=[$KOI_THEME_SEGMENTS]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=[dir git pins jobs duration exit k8s]") {
		t.Errorf("plugin segment not appended: %q", out)
	}
}

func TestConfigThemeSegments(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc,
		"config theme.segments 'exit dir'\necho live=[$KOI_THEME_SEGMENTS]\nconfig theme.segments\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=[exit dir]") {
		t.Errorf("segments not live: %q", out)
	}
	if !strings.Contains(out, `theme.segments = "exit dir" (KOI_THEME_SEGMENTS)`) {
		t.Errorf("show missing: %q", out)
	}

	_, errOut, _ := runConfigScript(t, filepath.Join(t.TempDir(), "koirc"),
		"config theme.segments 'dir;rm'\n")
	if !strings.Contains(errOut, "bad segment id") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestConfigThemeColor(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc, "config theme.color.dir cyan\necho live=[$KOI_THEME_COLOR_DIR]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=[cyan]") {
		t.Errorf("color not live: %q", out)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "KOI_THEME_COLOR_DIR=cyan\n" {
		t.Errorf("rc = %q", got)
	}

	_, errOut, _ := runConfigScript(t, rc, "config theme.color.dir rainbow\n")
	if !strings.Contains(errOut, "bad color") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestConfigThemeLayout(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc,
		"config theme.lines 1\nconfig theme.sep powerline\n"+
			"echo live=[$KOI_THEME_LINES $KOI_THEME_SEP]\nconfig theme.lines\nconfig theme.sep\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=[1 powerline]") {
		t.Errorf("layout not live: %q", out)
	}
	if !strings.Contains(out, "theme.lines = 1") || !strings.Contains(out, "theme.sep = powerline") {
		t.Errorf("show missing: %q", out)
	}

	_, errOut, _ := runConfigScript(t, rc, "config theme.lines 3\n")
	if !strings.Contains(errOut, "must be 1 or 2") {
		t.Errorf("stderr = %q", errOut)
	}
	_, errOut, _ = runConfigScript(t, rc, "config theme.sep fancy\n")
	if !strings.Contains(errOut, "plain or powerline") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestConfigThemeGuardsLastSegment(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	_, errOut, _ := runConfigScript(t, rc, "config theme.segments dir\nconfig theme.dir off\n")
	if !strings.Contains(errOut, "last segment") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestConfigCreatesXDGRCWhenNoneExists(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KOI_RC", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base) // UserHomeDir reads this on Windows

	var out strings.Builder
	err := RunReader(t.Context(), strings.NewReader("config lint native\n"), "test",
		interp.StdIO(nil, &out, &out))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(base, "config", "koi", "koirc"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "KOI_LINT=native\n" {
		t.Errorf("rc = %q", got)
	}
}

func TestConfigThemeRPrompt(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc,
		"config theme.rprompt 'time exit'\necho live=[$KOI_THEME_RPROMPT]\nconfig theme.rprompt\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=[time exit]") {
		t.Errorf("rprompt not live: %q", out)
	}
	if !strings.Contains(out, `theme.rprompt = "time exit" (KOI_THEME_RPROMPT)`) {
		t.Errorf("show missing: %q", out)
	}

	// Empty clears it; bad ids rejected.
	out, _, err = runConfigScript(t, rc, "config theme.rprompt ''\necho live=[$KOI_THEME_RPROMPT]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=[]") {
		t.Errorf("rprompt not cleared: %q", out)
	}
	_, errOut, _ := runConfigScript(t, rc, "config theme.rprompt 'a;b'\n")
	if !strings.Contains(errOut, "bad segment id") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestConfigThemeFrame(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc, "config theme.frame off\necho live=[$KOI_THEME_FRAME]\nconfig theme.frame\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=[off]") {
		t.Errorf("frame not live: %q", out)
	}
	if !strings.Contains(out, "theme.frame = off") {
		t.Errorf("show missing: %q", out)
	}
	_, errOut, _ := runConfigScript(t, rc, "config theme.frame sideways\n")
	if !strings.Contains(errOut, "on or off") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestConfigThemePreset(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	out, _, err := runConfigScript(t, rc,
		"config theme.preset spaceship\necho live=[$KOI_THEME $KOI_THEME_FRAME $KOI_THEME_COLOR_GIT]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=[p10k off magenta]") {
		t.Errorf("spaceship preset not live: %q", out)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"KOI_THEME_FRAME=off\n", "KOI_THEME_COLOR_DIR=cyan\n"} {
		if !strings.Contains(string(data), line) {
			t.Errorf("rc missing %q:\n%s", line, data)
		}
	}

	// Switching back to p10k resets every knob spaceship touched.
	out, _, err = runConfigScript(t, rc,
		"config theme.preset spaceship\nconfig theme.preset p10k\n"+
			"echo live=[$KOI_THEME $KOI_THEME_FRAME $KOI_THEME_COLOR_GIT]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "live=[p10k on ]") {
		t.Errorf("p10k preset did not reset: %q", out)
	}

	_, errOut, _ := runConfigScript(t, rc, "config theme.preset klingon\n")
	if !strings.Contains(errOut, "unknown preset") {
		t.Errorf("stderr = %q", errOut)
	}
}

// TestHeadlessSurfacesEmitNoEscapes pins the #90 degradation contract
// across invocation modes: when stdout is not a terminal (headless -c,
// pipes, scripts, CI — login or not), every styled surface emits plain
// bytes. One escape character in piped output is a regression.
func TestHeadlessSurfacesEmitNoEscapes(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "koirc")
	// `plugin browse` and `config theme` are the two surfaces that open a
	// form on a terminal (#90); headless they must print, not hang, and
	// not style. They are in this list rather than only in their own
	// tests so one place fails when a new surface forgets the rule.
	out, errOut, _ := runConfigScript(t, rc,
		"doctor\nzi\nzi help\ntool\ntool list golang\nconfig\nplugin browse\nconfig theme\n"+
			"echo hi | clip\nsessions\nblocks\nblocks show 1\nblocks search x\n")
	for name, s := range map[string]string{"stdout": out, "stderr": errOut} {
		if strings.Contains(s, "\x1b") {
			t.Errorf("%s carries escape sequences in headless mode: %q", name, s)
		}
	}
	// The words are all still there — styled and plain forms carry the
	// same content.
	for _, want := range []string{"⟨ Zi ⟩", "buffer ice modifiers", "terminal"} {
		if !strings.Contains(out, want) {
			t.Errorf("headless output missing %q", want)
		}
	}
}
