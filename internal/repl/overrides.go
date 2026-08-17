package repl

import (
	"context"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/builtins"
	"github.com/blairham/gish/internal/jobs"
)

// Builtins the interpreter claims but does not implement (#55).
//
// A name interp.IsBuiltin recognizes never reaches the ExecHandler, so
// an unimplemented one does not fall through to the program on the
// machine — it shadows it, and answers "unsupported builtin". kill and
// umask were both in that state, which is worse than being absent: the
// working /bin/kill and the shell's own umask were both unreachable.
//
// The route is the one jobs/fg/bg already use: rename the call so it
// arrives at the ExecHandler, where a builtin can return a real exit
// status. Returning the status from the CallHandler is not an option —
// the interpreter treats a CallHandler error as *fatal* and stops the
// script, so `kill somepid || echo nope` would take the shell down
// rather than run the fallback.
var nativeOverrides = map[string]string{
	"kill":   "__gish_kill",
	"umask":  "__gish_umask",
	"times":  "__gish_times",
	"newgrp": "__gish_newgrp",
}

// overrideCallHandler renames the overridden builtins. One map rather
// than a handler per name, so adding the next one is a line rather than
// another mechanism.
func overrideCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if to, ok := nativeOverrides[args[0]]; ok {
			args[0] = to
		}
		return next(ctx, args)
	}
}

// registerScriptOverrides wires the overridden builtins for the
// non-interactive paths.
//
// kill gets an empty job table: signaling a pid needs no table at all
// and that is what scripts use, while a %job spec correctly finds
// nothing to name. umask needs no shell state whatsoever.
func registerScriptOverrides() {
	builtins.Register("__gish_kill", (&jobs.Table{}).Kill)
	builtins.Register("__gish_umask", builtins.Umask)
	builtins.Register("__gish_times", builtins.Times)
	builtins.Register("__gish_newgrp", builtins.Newgrp)
}
