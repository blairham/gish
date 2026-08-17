package repl

import (
	"context"
	"errors"
	"fmt"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/builtins"
)

// printf goes through the CallHandler rather than the ExecHandler
// because the interpreter already claims the name: a builtin it
// recognizes never reaches the exec seam (see internal/builtins' package
// doc). This is the same route jobs/fg/bg and `config` take.
//
// Unlike those, this one replaces an implementation rather than adding a
// command, so it is wired into every runner — interactive, piped, and
// script. A printf that behaved differently in a script than at the
// prompt would be a worse bug than the one it fixes.
func printfCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "printf" {
			return next(ctx, args)
		}
		hc := interp.HandlerCtx(ctx)
		if err := builtins.Printf(hc.Stdout, args[1:]); err != nil {
			// A failed write means the reader went away — the head of a
			// pipeline that stopped reading. bash is silent there (printf
			// dies of SIGPIPE), and printing would both add noise and race
			// with whatever else writes stderr from the pipeline's other
			// goroutines. Only a bad format is worth a word.
			if !errors.Is(err, builtins.ErrWrite) {
				fmt.Fprintln(hc.Stderr, err)
			}
			return []string{"false"}, nil
		}
		return []string{"true"}, nil
	}
}
