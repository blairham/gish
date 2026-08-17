//go:build gishpanicprobe

package repl

import (
	"context"

	"mvdan.cc/sh/v3/interp"
)

// The panic the guard's end-to-end tests need, on demand.
//
// This exists because the first version of those tests induced a panic
// with a real substrate bug — a negated POSIX class in a pattern
// removal — and then #218 fixed the bug upstream, which left three
// tests asserting that a panic they could no longer cause was handled
// correctly. A test that needs a bug to exist stops working the moment
// someone does the right thing.
//
// So the trigger is ours now, and it is compiled only under the
// `gishpanicprobe` tag that cmd/gish's test build passes. A released
// binary does not contain this file, and `probeCommand` is not a name
// anybody types by accident.
//
// It sits at the innermost position of the CallHandler chain so the
// panic originates inside the interpreter, which is the whole point:
// the guard's job is to survive a panic raised *under* interp.Run,
// wherever it comes from.
// probeCommand is spelled to be unmistakable in a stack trace and
// impossible to type by accident. cmd/gish/spawn_test.go uses the same
// literal; it cannot import an unexported name across packages.
const probeCommand = "__gish_panic_probe"

func panicProbeCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if len(args) > 0 && args[0] == probeCommand {
			panic("panic probe: " + probeCommand)
		}
		return next(ctx, args)
	}
}
