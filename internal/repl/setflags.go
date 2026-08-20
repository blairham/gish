package repl

// `set` options given in argv (#426).
//
// bash accepts any single-letter `set` option on the command line —
// `bash -euxc 'cmd'` is what CI files, Makefiles and test harnesses
// write — and koi answered `unknown option "u" in "-uc"` plus a
// twenty-line usage dump, exit 2. As a failure it is maximally noisy:
// nameref.tests alone produced ~450 diff lines of repeated usage text
// against the bash suite, and any tool pointing $SHELL at koi got the
// same wall.
//
// The flags are applied by running the interpreter's own `set`, rather
// than by translating them into RunnerOptions here. That keeps one
// source of truth for the option table and, more importantly, keeps the
// interpreter's answer for an option koi does not implement: `set -v`
// reports "cannot turn verbose on: not implemented" and the shell
// carries on with status 0. Applying the same option through
// interp.Params would make an unimplemented option *fatal at startup*,
// so `koi -vc 'echo hi'` would print nothing and fail where bash runs
// the command — a strictly worse answer than the honest refusal.
//
// They run before the profile and the rc file, which is bash's order:
// an option given on the command line is in force for everything the
// session then reads.

import (
	"context"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// sessionSetFlags holds what argv asked for, in order. A package var
// rather than a parameter on six entry points, following Version and
// SetSessionSandbox — argv is process-wide state.
var sessionSetFlags []string

// SetSessionOptions records the `set` options parsed from argv.
func SetSessionOptions(flags []string) { sessionSetFlags = append([]string(nil), flags...) }

// applySessionOptions runs them in the session. Failures are the
// interpreter's to report: it has already written its refusal to stderr,
// and a shell that would not start because of one is the bug this
// replaces.
func applySessionOptions(ctx context.Context, runner *interp.Runner) {
	if len(sessionSetFlags) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("set")
	for _, f := range sessionSetFlags {
		b.WriteString(" ")
		b.WriteString(singleQuote(f))
	}
	_ = runHookSource(ctx, runner, b.String())
}
