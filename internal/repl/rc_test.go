package repl

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

// Tests never touch real user state: HOME, XDG_CONFIG_HOME, and GISH_RC
// are all redirected via t.Setenv + t.TempDir (AGENTS.md testing rule).

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRCPathPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GISH_RC", "")

	// Nothing exists: no rc.
	if got := rcPath(); got != "" {
		t.Errorf("rcPath() = %q, want empty", got)
	}

	// ~/.gishrc is the fallback.
	classic := writeFile(t, filepath.Join(home, ".gishrc"), "")
	if got := rcPath(); got != classic {
		t.Errorf("rcPath() = %q, want %q", got, classic)
	}

	// XDG location wins over ~/.gishrc.
	xdgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgDir)
	xdg := writeFile(t, filepath.Join(xdgDir, "gish", "gishrc"), "")
	if got := rcPath(); got != xdg {
		t.Errorf("rcPath() = %q, want %q", got, xdg)
	}

	// GISH_RC overrides everything, even when it doesn't exist yet.
	t.Setenv("GISH_RC", "/explicit/gishrc")
	if got := rcPath(); got != "/explicit/gishrc" {
		t.Errorf("rcPath() = %q, want explicit override", got)
	}
}

func TestLoadRCPersistsState(t *testing.T) {
	rc := writeFile(t, filepath.Join(t.TempDir(), "gishrc"), `
GISH_PROMPT='%W %?> '
greeting="from rc"
greet() { echo "$greeting"; }
`)
	t.Setenv("GISH_RC", rc)

	var out strings.Builder
	runner, err := interp.New(interp.StdIO(nil, &out, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	loadRC(context.Background(), runner)

	if got := shellVar(runner, "GISH_PROMPT", "def"); got != "%W %?> " {
		t.Errorf("GISH_PROMPT = %q", got)
	}
	// Functions and variables from the rc persist into the session.
	if err := runner.Run(context.Background(), parseLine(t, "greet")); err != nil {
		t.Fatalf("greet: %v", err)
	}
	if out.String() != "from rc\n" {
		t.Errorf("output = %q", out.String())
	}
}

func TestLoadRCBrokenFileIsNotFatal(t *testing.T) {
	rc := writeFile(t, filepath.Join(t.TempDir(), "gishrc"), "if then fi (broken")
	t.Setenv("GISH_RC", rc)

	runner := newTestRunner(t)
	loadRC(context.Background(), runner) // must not panic or exit

	// The runner must still work.
	if err := runner.Run(context.Background(), parseLine(t, "true")); err != nil {
		t.Fatalf("runner unusable after broken rc: %v", err)
	}
}

func TestLoadRCMissingFileIsSilent(t *testing.T) {
	t.Setenv("GISH_RC", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	runner := newTestRunner(t)
	loadRC(context.Background(), runner) // no file anywhere: no-op
}

func TestShellVarFallback(t *testing.T) {
	t.Parallel()

	runner := newTestRunner(t)
	if got := shellVar(runner, "GISH_PROMPT", "fallback"); got != "fallback" {
		t.Errorf("unset var = %q, want fallback", got)
	}
	if err := runner.Run(context.Background(), parseLine(t, `GISH_PROMPT="custom> "`)); err != nil {
		t.Fatal(err)
	}
	if got := shellVar(runner, "GISH_PROMPT", "fallback"); got != "custom> " {
		t.Errorf("set var = %q", got)
	}
}

func TestExpandPrompt(t *testing.T) {
	t.Parallel()

	info := promptInfo{
		username: "blair",
		host:     "mba",
		home:     "/home/blair",
		dir:      "/home/blair/dev/gish",
		exitCode: 7,
	}
	tests := []struct {
		format, want string
	}{
		{"gish$ ", "gish$ "},
		{"%u@%h ", "blair@mba "},
		{"%w $ ", "~/dev/gish $ "},
		{"%W $ ", "gish $ "},
		{"[%?] ", "[7] "},
		{"100%% ", "100% "},
		{"%x ", "%x "},   // unknown escape passes through
		{"end%", "end%"}, // trailing % is literal
	}
	for _, tt := range tests {
		if got := expandPrompt(tt.format, info); got != tt.want {
			t.Errorf("expandPrompt(%q) = %q, want %q", tt.format, got, tt.want)
		}
	}

	// At home, %w and %W both show ~.
	atHome := info
	atHome.dir = "/home/blair"
	if got := expandPrompt("%w|%W", atHome); got != "~|~" {
		t.Errorf("at home = %q, want ~|~", got)
	}
}
