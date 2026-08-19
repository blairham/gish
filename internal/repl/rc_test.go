package repl

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/interp"
)

// Tests never touch real user state: HOME, XDG_CONFIG_HOME, and KOI_RC
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
	t.Setenv("USERPROFILE", home) // UserHomeDir reads this on Windows
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("KOI_RC", "")

	// Nothing exists: no rc.
	if got := rcPath(); got != "" {
		t.Errorf("rcPath() = %q, want empty", got)
	}

	// ~/.koirc is the fallback.
	classic := writeFile(t, filepath.Join(home, ".koirc"), "")
	if got := rcPath(); got != classic {
		t.Errorf("rcPath() = %q, want %q", got, classic)
	}

	// XDG location wins over ~/.koirc.
	xdgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgDir)
	xdg := writeFile(t, filepath.Join(xdgDir, "koi", "koirc"), "")
	if got := rcPath(); got != xdg {
		t.Errorf("rcPath() = %q, want %q", got, xdg)
	}

	// KOI_RC overrides everything, even when it doesn't exist yet.
	t.Setenv("KOI_RC", "/explicit/koirc")
	if got := rcPath(); got != "/explicit/koirc" {
		t.Errorf("rcPath() = %q, want explicit override", got)
	}
}

func TestLoadRCPersistsState(t *testing.T) {
	rc := writeFile(t, filepath.Join(t.TempDir(), "koirc"), `
KOI_PROMPT='%W %?> '
greeting="from rc"
greet() { echo "$greeting"; }
`)
	t.Setenv("KOI_RC", rc)

	var out strings.Builder
	runner, err := interp.New(interp.StdIO(nil, &out, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	loadRC(context.Background(), runner)

	if got := shellVar(runner, "KOI_PROMPT", "def"); got != "%W %?> " {
		t.Errorf("KOI_PROMPT = %q", got)
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
	rc := writeFile(t, filepath.Join(t.TempDir(), "koirc"), "if then fi (broken")
	t.Setenv("KOI_RC", rc)

	runner := newTestRunner(t)
	loadRC(context.Background(), runner) // must not panic or exit

	// The runner must still work.
	if err := runner.Run(context.Background(), parseLine(t, "true")); err != nil {
		t.Fatalf("runner unusable after broken rc: %v", err)
	}
}

// The case #276 was filed about: an rc with one unreadable construct at
// the bottom used to lose *every* line of itself, so a single line koi
// could not parse cost the user their prompt, aliases and functions.
// bash keeps what it read; so does koi now.
func TestLoadRCKeepsWhatItCouldRead(t *testing.T) {
	rc := writeFile(t, filepath.Join(t.TempDir(), "koirc"), `
KOI_PROMPT='kept> '
greeting="from rc"
greet() { echo "$greeting"; }
if then fi
export NEVER=set
`)
	t.Setenv("KOI_RC", rc)

	var out strings.Builder
	runner, err := interp.New(interp.StdIO(nil, &out, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	loadRC(context.Background(), runner)

	if got := shellVar(runner, "KOI_PROMPT", "def"); got != "kept> " {
		t.Errorf("KOI_PROMPT = %q, want the value set before the bad line", got)
	}
	if err := runner.Run(context.Background(), parseLine(t, "greet")); err != nil {
		t.Fatalf("greet: %v", err)
	}
	if out.String() != "from rc\n" {
		t.Errorf("output = %q, want the function defined before the bad line", out.String())
	}
	// And nothing after the error runs, which is the other half of
	// matching bash: the rc is truncated at the error, not skipped past.
	if got := shellVar(runner, "NEVER", ""); got != "" {
		t.Errorf("NEVER = %q, want nothing — it is after the syntax error", got)
	}
}

func TestLoadRCMissingFileIsSilent(t *testing.T) {
	t.Setenv("KOI_RC", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir()) // UserHomeDir reads this on Windows

	runner := newTestRunner(t)
	loadRC(context.Background(), runner) // no file anywhere: no-op
}

func TestShellVarFallback(t *testing.T) {
	t.Parallel()

	runner := newTestRunner(t)
	if got := shellVar(runner, "KOI_PROMPT", "fallback"); got != "fallback" {
		t.Errorf("unset var = %q, want fallback", got)
	}
	if err := runner.Run(context.Background(), parseLine(t, `KOI_PROMPT="custom> "`)); err != nil {
		t.Fatal(err)
	}
	if got := shellVar(runner, "KOI_PROMPT", "fallback"); got != "custom> " {
		t.Errorf("set var = %q", got)
	}
}

func TestExpandPrompt(t *testing.T) {
	t.Parallel()

	info := promptInfo{
		username: "blair",
		host:     "mba",
		home:     filepath.FromSlash("/home/blair"),
		dir:      filepath.FromSlash("/home/blair/dev/koi"),
		exitCode: 7,
	}
	tests := []struct {
		format, want string
	}{
		{"koi$ ", "koi$ "},
		{"%u@%h ", "blair@mba "},
		{"%w $ ", filepath.FromSlash("~/dev/koi") + " $ "},
		{"%W $ ", "koi $ "},
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
	atHome.dir = filepath.FromSlash("/home/blair")
	if got := expandPrompt("%w|%W", atHome); got != "~|~" {
		t.Errorf("at home = %q, want ~|~", got)
	}
}

func TestExpandPromptSegments(t *testing.T) {
	t.Parallel()

	info := promptInfo{
		segment: func(id string) string {
			if id == "git" {
				return "main !1"
			}
			return ""
		},
	}
	tests := []struct {
		format, want string
	}{
		{"%p{git} $ ", "main !1 $ "},
		{"%p{nope}$ ", "$ "},         // unknown segment renders empty
		{"%p $ ", "%p $ "},           // no braces: literal
		{"%p{open $ ", "%p{open $ "}, // unclosed brace: everything literal
	}
	for _, tt := range tests {
		if got := expandPrompt(tt.format, info); got != tt.want {
			t.Errorf("expandPrompt(%q) = %q, want %q", tt.format, got, tt.want)
		}
	}

	// nil segment func renders empty rather than panicking.
	if got := expandPrompt("x%p{git}y", promptInfo{}); got != "xy" {
		t.Errorf("nil segment = %q, want %q", got, "xy")
	}
}

// TestShellVarReadsEnvironment pins the contract that `KOI_THEME=p10k
// koi` works: settings come from shell variables first, then the
// inherited environment. Before this, only rc assignments were seen —
// found by the #102 benchmark harness, which could not set a prompt.
func TestShellVarReadsEnvironment(t *testing.T) {
	runner, err := interp.New(interp.Env(expand.ListEnviron(
		"KOI_THEME=p10k", "KOI_EMPTY=", "PATH=/usr/bin")))
	if err != nil {
		t.Fatal(err)
	}
	if got := shellVar(runner, "KOI_THEME", "plain"); got != "p10k" {
		t.Errorf("env setting ignored: %q", got)
	}
	// An empty env value is not a setting; the fallback stands.
	if got := shellVar(runner, "KOI_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("empty env value = %q", got)
	}
	if got := shellVar(runner, "KOI_UNSET", "fallback"); got != "fallback" {
		t.Errorf("unset = %q", got)
	}
	// A shell assignment beats the environment: `config` and rc win.
	if err := runner.Run(t.Context(), parseLine(t, `KOI_THEME=starship`)); err != nil {
		t.Fatal(err)
	}
	if got := shellVar(runner, "KOI_THEME", "plain"); got != "starship" {
		t.Errorf("shell assignment did not win: %q", got)
	}
}
