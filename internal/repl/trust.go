package repl

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// The trust command (#12): the user-facing side of the env-diff trust
// model. `trust` shows what is pending for the current directory and
// what has been allowed; `trust allow` records and applies the pending
// proposal; `trust revoke [dir]` withdraws it (reverting a live diff).

const trustUsage = `usage: trust [allow | revoke [dir]]

  trust              show the pending env proposal and allowed directories
  trust allow        allow the pending proposal: applied now and remembered
                     for this exact diff — a changed diff pends again
  trust revoke [dir] withdraw trust for dir (default: current directory),
                     reverting the diff if it is live`

// trustCallHandler intercepts `trust` before execution, config-style.
func trustCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "trust" {
			return next(ctx, args)
		}
		return runTrust(ctx, interp.HandlerCtx(ctx), args[1:]), nil
	}
}

// sessionRunnerRef holds the runner of whichever session is running —
// the interactive loop, a piped one, or a script. A process has one
// session, so a package-level handle is the honest shape; it is atomic
// because the test binary runs several sessions at once through
// RunReader, and `go test -race` is right to call that a race.
var sessionRunnerRef atomic.Pointer[interp.Runner]

// setSessionRunner publishes the runner for the builtins that answer
// questions *about* the session.
func setSessionRunner(r *interp.Runner) { sessionRunnerRef.Store(r) }

// sessionRunner returns that runner, or nil before one exists. The
// builtins that need it (trust, declare -F) run on every path, not only
// the interactive one, since an init script asks the same questions
// wherever it is sourced.
func sessionRunner() *interp.Runner { return sessionRunnerRef.Load() }

func runTrust(ctx context.Context, hc interp.HandlerContext, args []string) []string {
	fail := func(err error) []string {
		fmt.Fprintln(hc.Stderr, "trust:", err)
		return []string{"false"}
	}
	if envMgr == nil || sessionRunner() == nil {
		return fail(fmt.Errorf("env plugins are not available in this session"))
	}
	runner := sessionRunner()

	switch {
	case len(args) == 0:
		showTrust(hc, runner)
		return reviewPending(ctx, hc, runner)
	case args[0] == "help" || args[0] == "-h" || args[0] == "--help":
		fmt.Fprintln(hc.Stdout, trustUsage)
		return []string{"true"}
	case args[0] == "allow" && len(args) == 1:
		msg, err := envMgr.allowPending(ctx, runner)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(hc.Stdout, msg)
		return []string{"true"}
	case args[0] == "revoke" && len(args) <= 2:
		dir := runner.Dir
		if len(args) == 2 {
			dir = args[1]
		}
		removed, err := envMgr.revokeDir(ctx, runner, dir)
		if err != nil {
			return fail(err)
		}
		if !removed {
			fmt.Fprintf(hc.Stdout, "nothing was trusted for %s\n", displayPath(dir))
			return []string{"true"}
		}
		fmt.Fprintf(hc.Stdout, "revoked %s\n", displayPath(dir))
		return []string{"true"}
	default:
		return fail(fmt.Errorf("unknown arguments %q\n%s", strings.Join(args, " "), trustUsage))
	}
}

// reviewPending turns a pending proposal into an interactive decision
// on a real terminal (#90); anywhere else the printed summary stands
// and the trust allow/revoke commands do the work.
func reviewPending(ctx context.Context, hc interp.HandlerContext, runner *interp.Runner) []string {
	envMgr.mu.Lock()
	pending := envMgr.pending
	envMgr.mu.Unlock()
	choose := interactiveChooser(hc.Stdin, hc.Stdout)
	if pending == nil || choose == nil {
		return []string{"true"}
	}
	answer, ok := choose("apply this environment diff?", []chooseOption{
		{"a", "allow — apply now and remember this exact diff"},
		{"n", "not now — keep it pending"},
		{"r", "revoke " + displayPath(pending.forDir) + " — stop proposing here"},
	})
	if !ok || answer == "n" {
		return []string{"true"}
	}
	switch answer {
	case "a":
		msg, err := envMgr.allowPending(ctx, runner)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "trust:", err)
			return []string{"false"}
		}
		fmt.Fprintln(hc.Stdout, msg)
	case "r":
		if _, err := envMgr.revokeDir(ctx, runner, pending.forDir); err != nil {
			fmt.Fprintln(hc.Stderr, "trust:", err)
			return []string{"false"}
		}
		fmt.Fprintf(hc.Stdout, "revoked %s\n", displayPath(pending.forDir))
	}
	return []string{"true"}
}

func showTrust(hc interp.HandlerContext, runner *interp.Runner) {
	envMgr.mu.Lock()
	pending, active := envMgr.pending, envMgr.active
	envMgr.mu.Unlock()

	switch {
	case pending != nil:
		fmt.Fprintf(hc.Stdout, "pending: plugin %q for %s\n", pending.plugin, displayPath(pending.forDir))
		for _, name := range slices.Sorted(maps.Keys(pending.set)) {
			fmt.Fprintf(hc.Stdout, "  set   %s=%q\n", name, pending.set[name])
		}
		for _, name := range pending.unset {
			fmt.Fprintf(hc.Stdout, "  unset %s\n", name)
		}
		if len(pending.stripped) > 0 {
			fmt.Fprintf(hc.Stdout, "  (stripped by policy: %s)\n", strings.Join(pending.stripped, " "))
		}
		fmt.Fprintln(hc.Stdout, "run `trust allow` to apply and remember this exact diff")
	case active != nil:
		fmt.Fprintf(hc.Stdout, "active: plugin %q for %s (%d variable(s))\n",
			active.plugin, displayPath(active.forDir), len(active.saved))
	default:
		fmt.Fprintf(hc.Stdout, "nothing pending for %s\n", displayPath(runner.Dir))
	}

	if entries := envMgr.trust.Entries(); len(entries) > 0 {
		fmt.Fprintln(hc.Stdout, "allowed:")
		for _, e := range entries {
			fmt.Fprintf(hc.Stdout, "  %s  (%s, %.12s)\n", displayPath(e.Dir), e.Plugin, e.Hash)
		}
	}
}
