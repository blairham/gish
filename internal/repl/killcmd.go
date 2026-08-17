package repl

import (
	"context"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/builtins"
	"github.com/blairham/gish/internal/jobs"
)

// kill reaches the native implementation the same way jobs/fg/bg do —
// the CallHandler renames it so it arrives at the ExecHandler, where a
// builtin can return a real exit status.
//
// Returning the status from the CallHandler itself is not an option: the
// interpreter treats a CallHandler error as *fatal* and stops the
// script, so `kill somepid || echo nope` would take the shell down
// instead of running the fallback. Rewriting to true/false would lose
// the difference between a usage error (bash exits 2) and a failed
// signal (1), and getting exit statuses subtly wrong is how a script
// silently takes the wrong branch.
//
// Installed on every path. `kill` with a plain pid needs no job table at
// all, and it is what scripts use; only %job specs need one, and those
// report "no such job" where there is nothing to look in.
func killCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] == "kill" {
			args[0] = "__gish_kill"
		}
		return next(ctx, args)
	}
}

// registerScriptKill gives the non-interactive paths a kill bound to an
// empty table: pids work, and a %job spec finds nothing to name.
func registerScriptKill() {
	builtins.Register("__gish_kill", (&jobs.Table{}).Kill)
}
