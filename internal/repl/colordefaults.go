package repl

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// Color-friendly defaults (#54).
//
// koi shipped none, so `ls` was bare and man pages were monochrome for
// anyone who had not written the incantations themselves. That is the
// gap between "the shell works" and "the shell is pleasant on the first
// run", and #105 puts fish-grade defaults on the adoption path rather
// than in the nice-to-have pile.
//
// Three rules shape all of it:
//
// Nothing here forks. Startup is budgeted (#37, ~4ms to first prompt),
// so asking `ls --version` which flavor it is would cost more than the
// whole prompt. The platform is known at compile time and that is
// enough: BSD ls reads CLICOLOR from the environment, GNU ls needs
// --color=auto on the command line.
//
// Nothing here overrides the user. A value already in the environment
// is theirs, and these are applied before the rc file runs so anything
// in it wins by arriving later. That ordering is the whole reason this
// is not simply written into the default rc.
//
// Nothing here defeats NO_COLOR. Both mechanisms are tty-aware by
// construction — CLICOLOR and --color=auto each check isatty, so `ls |
// cat` stays plain without koi arranging it — and when color is
// unwanted outright, none of it is set at all.

// lessTermcap is the man-page palette, applied through less's termcap
// hooks. This is the widely-copied incantation rather than an invention:
// man asks less to render bold, underline and standout, and less asks
// the terminal, so overriding those three is the supported way to color
// a man page without a pager wrapper.
var lessTermcap = map[string]string{
	"LESS_TERMCAP_md": "\x1b[1;36m", // bold: headings and names — cyan
	"LESS_TERMCAP_me": "\x1b[0m",
	"LESS_TERMCAP_us": "\x1b[4;32m", // underline: arguments — green
	"LESS_TERMCAP_ue": "\x1b[0m",
	"LESS_TERMCAP_so": "\x1b[1;33m", // standout: the prompt line and matches
	"LESS_TERMCAP_se": "\x1b[0m",
}

// applyColorDefaults sets the environment and aliases that make ls and
// man readable, without touching anything the user has already decided.
//
// Called before the rc runs and before any command, so an rc assignment
// or alias replaces whatever is set here.
func applyColorDefaults(ctx context.Context, runner *interp.Runner) {
	if !wantColorDefaults(runner) {
		return
	}

	assign := make([]string, 0, len(lessTermcap)+1)
	for name, value := range lessTermcap {
		if shellVar(runner, name, "") == "" {
			assign = append(assign, fmt.Sprintf("export %s=%s", name, shellQuoteValue(value)))
		}
	}
	// BSD ls colors when CLICOLOR is set and stdout is a terminal, so on
	// those systems no alias is needed at all — which also means `command
	// ls` and scripts calling ls behave identically to before.
	if runtime.GOOS != "linux" && shellVar(runner, "CLICOLOR", "") == "" {
		assign = append(assign, "export CLICOLOR=1")
	}
	// GNU ls has no environment switch for this; --color=auto is the
	// only way, and it is tty-aware in exactly the same manner.
	if runtime.GOOS == "linux" {
		assign = append(assign, `alias ls='ls --color=auto'`)
	}
	// groff ≥1.23 writes SGR color itself, which bypasses less's termcap
	// hooks — on modern Linux the palette above would silently do
	// nothing. MANROFFOPT is man-db's knob for formatter flags, and -c
	// selects the classic overstrike output that LESS_TERMCAP colors.
	// macOS man is mandoc-backed and needs nothing.
	if runtime.GOOS == "linux" && shellVar(runner, "MANROFFOPT", "") == "" {
		assign = append(assign, "export MANROFFOPT='-c'")
	}
	if len(assign) == 0 {
		return
	}

	src := strings.Join(assign, "\n")
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "koi")
	if err != nil {
		return
	}
	if err := runner.Run(ctx, file); err != nil {
		// Defaults are a courtesy; a shell that cannot set them still
		// has to start.
		fmt.Fprintf(os.Stderr, "koi: color defaults: %v\n", err)
	}
}

// wantColorDefaults reports whether to apply them at all.
//
// NO_COLOR and a dumb terminal are the same refusal the prompt and every
// styled surface already honor, and KOI_COLOR_DEFAULTS=off is the
// escape hatch for someone who wants koi's other defaults but not
// these — the same shape as KOI_TOOLS and KOI_JUMP.
func wantColorDefaults(runner *interp.Runner) bool {
	if shellVar(runner, "KOI_COLOR_DEFAULTS", "on") == "off" {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	switch os.Getenv("TERM") {
	case "", "dumb":
		return false
	}
	return true
}

// shellQuoteValue renders a value for an assignment. The termcap strings
// contain escape characters, so they are single-quoted rather than
// pasted raw into shell source.
func shellQuoteValue(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
