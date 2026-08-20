package repl

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

func colorRunner(t *testing.T) (*interp.Runner, *bytes.Buffer) {
	t.Helper()
	// The assertions read what applyColorDefaults declined to set, and
	// the runner inherits this process's environment — where a colorful
	// dev shell exports these very variables (#435). Hermetic means
	// unsetting them, not hoping the machine is as bare as CI's; the
	// t.Setenv first registers the restore.
	for _, name := range []string{
		"CLICOLOR", "LSCOLORS", "LS_COLORS",
		"LESS_TERMCAP_md", "LESS_TERMCAP_me", "LESS_TERMCAP_us",
		"LESS_TERMCAP_ue", "LESS_TERMCAP_so", "LESS_TERMCAP_se",
	} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	var out bytes.Buffer
	r, err := interp.New(interp.StdIO(nil, &out, &out))
	if err != nil {
		t.Fatal(err)
	}
	return r, &out
}

func runLine(t *testing.T, r *interp.Runner, src string) string {
	t.Helper()
	var out bytes.Buffer
	interp.StdIO(nil, &out, &out)(r) //nolint:errcheck // in-memory
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "t")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Run(context.Background(), file)
	return out.String()
}

// The man-page palette is the point of the feature: without these, man
// renders monochrome however capable the terminal is.
func TestColorDefaultsSetTheManPalette(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")

	r, _ := colorRunner(t)
	applyColorDefaults(context.Background(), r)
	if got := runLine(t, r, `printf '%s' "$LESS_TERMCAP_md"`); got == "" {
		t.Error("LESS_TERMCAP_md unset: man pages stay monochrome")
	}
}

// NO_COLOR is the same refusal every other styled surface honors, and it
// has to reach this one too — a shell that colors ls after being told
// not to is worse than one that never colored it.
func TestColorDefaultsRespectNoColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")

	r, _ := colorRunner(t)
	applyColorDefaults(context.Background(), r)
	for _, name := range []string{"LESS_TERMCAP_md", "CLICOLOR"} {
		if got := runLine(t, r, `printf '%s' "$`+name+`"`); got != "" {
			t.Errorf("%s = %q under NO_COLOR", name, got)
		}
	}
}

func TestColorDefaultsRespectDumbTerminal(t *testing.T) {
	t.Setenv("TERM", "dumb")
	t.Setenv("NO_COLOR", "")

	r, _ := colorRunner(t)
	applyColorDefaults(context.Background(), r)
	if got := runLine(t, r, `printf '%s' "$LESS_TERMCAP_md"`); got != "" {
		t.Errorf("LESS_TERMCAP_md = %q on a dumb terminal", got)
	}
}

// A value the user already set is theirs. This is what makes applying
// the defaults before the rc safe rather than presumptuous.
func TestColorDefaultsDoNotOverride(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")

	r, _ := colorRunner(t)
	runLine(t, r, `export LESS_TERMCAP_md='MINE'`)
	applyColorDefaults(context.Background(), r)
	if got := runLine(t, r, `printf '%s' "$LESS_TERMCAP_md"`); got != "MINE" {
		t.Errorf("LESS_TERMCAP_md = %q, want the user's own value", got)
	}
}

func TestColorDefaultsCanBeTurnedOff(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")

	r, _ := colorRunner(t)
	runLine(t, r, `export KOI_COLOR_DEFAULTS=off`)
	applyColorDefaults(context.Background(), r)
	if got := runLine(t, r, `printf '%s' "$LESS_TERMCAP_md"`); got != "" {
		t.Errorf("LESS_TERMCAP_md = %q with KOI_COLOR_DEFAULTS=off", got)
	}
}

// ls is colored by whichever mechanism the platform's ls actually reads:
// BSD ls takes CLICOLOR from the environment, GNU ls needs --color=auto
// on the command line. Both are tty-aware themselves, so `ls | cat`
// stays plain without koi arranging anything.
func TestColorDefaultsColorLsPerPlatform(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")

	r, _ := colorRunner(t)
	applyColorDefaults(context.Background(), r)
	clicolor := runLine(t, r, `printf '%s' "$CLICOLOR"`)
	if runtime.GOOS == "linux" {
		if clicolor != "" {
			t.Errorf("CLICOLOR = %q on linux, where GNU ls ignores it", clicolor)
		}
		return
	}
	if clicolor != "1" {
		t.Errorf("CLICOLOR = %q, want 1 so BSD ls colors a terminal", clicolor)
	}
}

// The palette only works when the formatter still emits termcap-shaped
// output: groff ≥1.23 writes SGR itself and bypasses less's hooks, so on
// Linux MANROFFOPT=-c asks man-db for the classic overstrike output the
// palette colors. macOS man is mandoc-backed and needs nothing.
func TestColorDefaultsKeepManTermcapShapedPerPlatform(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")

	r, _ := colorRunner(t)
	applyColorDefaults(context.Background(), r)
	got := runLine(t, r, `printf '%s' "$MANROFFOPT"`)
	if runtime.GOOS == "linux" {
		if got != "-c" {
			t.Errorf("MANROFFOPT = %q on linux, want -c so groff's SGR output does not bypass the palette", got)
		}
		return
	}
	if got != "" {
		t.Errorf("MANROFFOPT = %q on %s, where mandoc needs nothing", got, runtime.GOOS)
	}
}
