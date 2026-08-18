package repl

import (
	"context"
	"os"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/jobs"
)

// `$-`, the shell's option flags (#159).
//
// This one variable gates whole init scripts. fzf's key bindings begin
// `if [[ $- =~ i ]]`, and `case $- in *i*)` guards the interactive half
// of a great many rc files — including the ones people have carried
// since the nineties. An empty `$-` means every one of those blocks is
// skipped, silently, and the tool appears to have installed nothing.
//
// What lands here is only the half the *shell* knows. Whether errexit or
// nounset is on is the interpreter's business and changes on any line, so
// `$-` is rendered there (interp.optionFlags) from the live option table,
// with these letters unioned in — which is the whole of #265: this file
// used to answer alone, and answered the same string forever, so the probe
// `case $- in *e*)` reported errexit off in a shell that had just set it.
//
// It is supplied through the base environment rather than as a shell
// variable because a variable named `-` cannot be written by any
// assignment syntax. Going in through the environment also keeps it out
// of children: Each does not yield it, so no subprocess is handed an env
// entry named "-", which nothing in the world expects to see.

// invocation is how the shell was started, which bash reports as the last
// letter of `$-`: `c` for -c, `s` for commands read from standard input,
// and neither for a script named on the command line.
type invocation byte

const (
	invokedScript  invocation = 0
	invokedCommand invocation = 'c'
	invokedStdin   invocation = 's'
)

// sessionFlags is what only the shell around the interpreter can answer.
//
// Each field is claimed rather than inferred, because the honest answer
// differs by path and inferring would overclaim: `koi -ic` is interactive
// and sources the rc, but runs no line editor, so it has neither history
// expansion nor job control — and saying otherwise in a variable whose
// entire purpose is to be probed is how a caller takes the wrong branch.
type sessionFlags struct {
	interactive bool // -i, or a session on a terminal
	jobControl  bool // process groups and terminal handoff are live (#5)
	histExpand  bool // `!!` and friends are being expanded (#96)
	invocation  invocation
}

// shellFlags reports this session's letters. Only the ones koi can
// honestly claim appear; ordering is left to the interpreter, which has
// the other half of the string.
//
// bash's `h` (hashall) is the one deliberate omission, and it is why the
// tests compare probe answers rather than the whole string: bash reports
// it in every shell, but koi's `set` refuses `+h` (#245), so claiming it
// would put a letter in `$-` that a caller cannot turn off. Absent is a
// safe answer for a probe; present-and-unchangeable is not.
func shellFlags(f sessionFlags) string {
	flags := "B" // brace expansion, always on
	if f.interactive {
		flags += "i"
	}
	if f.histExpand {
		flags += "H"
	}
	if f.jobControl && jobs.Supported() {
		flags += "m"
	}
	if f.invocation != invokedScript {
		flags += string(rune(f.invocation))
	}
	return flags
}

// flagsEnviron answers `$-` and delegates everything else.
type flagsEnviron struct {
	base  expand.Environ
	flags string
}

func (e flagsEnviron) Get(name string) expand.Variable {
	if name == "-" {
		return expand.Variable{Set: true, Kind: expand.String, Str: e.flags}
	}
	return e.base.Get(name)
}

// Each deliberately does not yield "-": it is the shell's own state,
// not an environment variable, and exporting it would put a nameless
// oddity in every child process's environment.
func (e flagsEnviron) Each(fn func(string, expand.Variable) bool) { e.base.Each(fn) }

// sessionEnv builds the runner's base environment with `$-` in place.
func sessionEnv(f sessionFlags) expand.Environ {
	return flagsEnviron{
		base:  expand.ListEnviron(os.Environ()...),
		flags: shellFlags(f),
	}
}

// Shell identity (#120), settled by what the ecosystem matrix (#159)
// measured rather than by taste.
//
// Tools use BASH_VERSION and BASH_VERSINFO as **feature probes**, not
// as questions about who they are talking to. fzf is the clearest case:
// `((BASH_VERSINFO[0] < 4))` chooses between binding Ctrl-T to a
// readline *macro* — a string of editing commands including
// shell-expand-line, which koi deliberately does not emulate — and
// binding it with `bind -x`, which koi implements. With the variable
// unset the arithmetic reads 0, so koi was handed the legacy path it
// cannot run, in order to avoid claiming a capability it has.
//
// So: koi answers the feature probe with a modern bash, because the
// features those probes gate are ones it implements. It does not answer
// the *identity* question that way — $0 stays `koi`, since that is
// what a script re-execs and what a user sees, and lying there would be
// a lie a program could act on. KOI_VERSION is set alongside, so
// anything that wants to know exactly what it is talking to can ask.
//
// The honest summary, which docs/porting.md carries: koi claims bash's
// interface, not bash's identity.
const (
	claimedBashVersion  = "5.2.21(1)-release"
	claimedBashMajor    = "5"
	claimedBashMinor    = "2"
	claimedBashPatch    = "21"
	claimedBashBuild    = "1"
	claimedBashRelease  = "release"
	claimedBashMachType = "koi"
)

// shellIdentitySource is the assignment set run into a fresh session.
// BASH_VERSINFO has to be an assignment rather than an environment
// entry because it is an indexed array, and arrays do not survive the
// environment — which is also why bash itself does not export it.
func shellIdentitySource(version string) string {
	if version == "" {
		version = "dev"
	}
	return strings.Join([]string{
		"BASH_VERSION=" + singleQuote(claimedBashVersion),
		"BASH_VERSINFO=(" + strings.Join([]string{
			claimedBashMajor, claimedBashMinor, claimedBashPatch,
			claimedBashBuild, claimedBashRelease, singleQuote(claimedBashMachType),
		}, " ") + ")",
		"KOI_VERSION=" + singleQuote(version),
	}, "\n")
}

// Version is koi's own version, stamped by main at build time and
// reported to the session as KOI_VERSION.
var Version = "dev"

// declareShellIdentity runs the identity assignments in a session.
func declareShellIdentity(ctx context.Context, runner *interp.Runner) {
	runHookSource(ctx, runner, shellIdentitySource(Version)) //nolint:errcheck // identity is best effort
}
