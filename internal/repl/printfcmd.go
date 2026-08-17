package repl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

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

		opts, ok := parsePrintfArgs(args[1:])
		if !ok {
			fmt.Fprintln(hc.Stderr, builtins.ErrUsage)
			return printfStatus(2), nil
		}
		if !opts.assign {
			if err := builtins.Printf(hc.Stdout, hc.Stderr, opts.operands); err != nil {
				// bash separates the two: no format at all is a usage
				// error and exits 2, a bad conversion exits 1.
				if errors.Is(err, builtins.ErrUsage) {
					fmt.Fprintln(hc.Stderr, err)
					return printfStatus(2), nil
				}
				// A failed write means the reader went away — the head of a
				// pipeline that stopped reading. bash is silent there (printf
				// dies of SIGPIPE), and printing would both add noise and race
				// with whatever else writes stderr from the pipeline's other
				// goroutines. A bad number has already been reported by the
				// builtin, in bash's order; only a bad format is left to say.
				if !errors.Is(err, builtins.ErrWrite) && !errors.Is(err, builtins.ErrBadNumber) {
					fmt.Fprintln(hc.Stderr, err)
				}
				return []string{"false"}, nil
			}
			return []string{"true"}, nil
		}
		return printfAssign(hc, opts)
	}
}

// printfOpts is a parsed `printf` invocation. assign distinguishes "no
// -v was given" from "-v was given an empty name", which bash rejects.
type printfOpts struct {
	assign   bool
	target   string
	operands []string
}

// parsePrintfArgs splits leading options from the format and its
// arguments. ok is false only for a usage error.
//
// Options stop at the format, which is why `printf "%s" -v x` prints
// "-vx" rather than assigning: -v is an option only before the format.
// An unrecognized dash-argument is therefore treated as the format,
// which is both what bash's callers rely on and what gish did before
// there was any option parsing here at all.
func parsePrintfArgs(args []string) (printfOpts, bool) {
	var o printfOpts
	i := 0
	for ; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--":
			i++
			o.operands = args[i:]
			return o, true
		case arg == "-v":
			i++
			if i >= len(args) {
				return o, false
			}
			// A repeated -v takes the last name, as bash does.
			o.assign, o.target = true, args[i]
		case strings.HasPrefix(arg, "-v"):
			o.assign, o.target = true, arg[2:]
		default:
			o.operands = args[i:]
			return o, true
		}
	}
	o.operands = nil
	return o, true
}

// printfAssign renders into a buffer and hands the assignment back to
// the interpreter to run, which is how `config` applies a setting to the
// live session: a CallHandler cannot reach the variable scope itself,
// and the scope is the whole point — bash's `printf -v` inside a
// function with a `local` of that name writes the local.
func printfAssign(hc interp.HandlerContext, opts printfOpts) ([]string, error) {
	if !validAssignTarget(opts.target) {
		// bash's own wording, because scripts grep for it.
		fmt.Fprintf(hc.Stderr, "printf: `%s': not a valid identifier\n", opts.target)
		return printfStatus(2), nil
	}
	if len(opts.operands) == 0 {
		fmt.Fprintln(hc.Stderr, builtins.ErrUsage)
		return printfStatus(2), nil
	}

	var buf bytes.Buffer
	perr := builtins.Printf(&buf, hc.Stderr, opts.operands)

	// The value is quoted; the target is validated but not quoted,
	// because a subscript has to keep being evaluated — bash resolves
	// `arr[$i]` and `arr[1+1]` at assignment time, so quoting it would
	// turn a working line into a literal key. validAssignTarget is what
	// keeps that from being a way to smuggle in a command: it demands
	// the closing bracket be the final character, which is exactly what
	// makes bash reject `x[1]$(...)[2]`.
	quoted, qerr := syntax.Quote(buf.String(), syntax.LangBash)
	if qerr != nil {
		// Unquotable output is a null byte or invalid UTF-8. bash stores
		// it; we cannot express it through eval, so say so rather than
		// assigning something subtly different.
		fmt.Fprintf(hc.Stderr, "printf: cannot assign to %s: %v\n", opts.target, qerr)
		return printfStatus(1), nil
	}
	assign := opts.target + "=" + quoted

	if perr != nil {
		// A bad conversion still assigns what was rendered before it —
		// `printf -v x "%d" notanum` leaves x as "0" and exits 1 — so
		// the assignment runs and carries the failing status with it.
		// A bad number has already said so on stderr; anything else
		// still needs reporting.
		if !errors.Is(perr, builtins.ErrBadNumber) {
			fmt.Fprintln(hc.Stderr, perr)
		}
		return []string{"eval", assign + "; (exit 1)"}, nil
	}
	return []string{"eval", assign}, nil
}

// printfStatus returns a rewrite that exits with status, for the codes
// `false` cannot express. The subshell is the carrier: it reports the
// status without ending the session, and nothing else in it can.
func printfStatus(status int) []string {
	return []string{"eval", fmt.Sprintf("(exit %d)", status)}
}

// validAssignTarget reports whether name is something bash's `printf -v`
// would accept: a plain identifier, or an identifier with a subscript
// whose matching bracket is the last character.
//
// That last clause is the load-bearing one. `x[1]$(echo pwned)[2]` ends
// in a bracket and starts with an identifier, and bash rejects it —
// balance alone is not enough, the subscript has to be the whole tail.
func validAssignTarget(name string) bool {
	if name == "" {
		return false
	}
	if c := name[0]; !isNameStart(c) {
		return false
	}
	i := 1
	for i < len(name) && isNameChar(name[i]) {
		i++
	}
	if i == len(name) {
		return true
	}
	if name[i] != '[' {
		return false
	}
	depth := 0
	for j := i; j < len(name); j++ {
		switch name[j] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return j == len(name)-1
			}
		}
	}
	return false
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}
