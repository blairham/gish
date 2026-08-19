// Package builtins hosts koi-native builtins, layered over the
// interpreter through mvdan's ExecHandler middleware: the handler fires
// exactly when a command name is not an interpreter builtin, which makes
// it the natural extension seam. This package is its first occupant; job
// control's jobs/fg/bg (#5) and plugin-provided commands (M3
// CommandProvider) build on the same mechanism.
//
// Name policy: a koi builtin shadows PATH binaries of the same name
// (like any shell builtin); pick names accordingly. Constraint: a name
// the interpreter recognizes (interp.IsBuiltin — the POSIX/bash set,
// including unimplemented ones like help/jobs/fg/bg) never reaches this
// handler; taking such names over requires intercepting the call before
// builtin dispatch (the CallHandler route — see #5).
package builtins

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// Func is a koi-native builtin. args excludes the command name; output
// goes to the handler context's stdio, never the process's.
type Func func(ctx context.Context, hc interp.HandlerContext, args []string) error

var registry = map[string]Func{}

func init() {
	// Populated here rather than in the literal: listBuiltins reaches
	// back into the registry via Native, which a literal would make an
	// initialization cycle.
	//
	// No "help" alias: the interpreter recognizes that name, so it needs
	// the CallHandler route — and it lives there (#196, repl/helpcmd.go)
	// because `help config` dispatches back into the handler chain.
	registry["builtins"] = listBuiltins
}

// ExecHandler is the middleware that dispatches koi-native builtins and
// passes everything else on (ultimately to the PATH-exec handler).
func ExecHandler(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if fn, ok := registry[args[0]]; ok {
			return fn(ctx, interp.HandlerCtx(ctx), args[1:])
		}
		return next(ctx, args)
	}
}

// Register adds a koi-native builtin; called from startup wiring, before
// any command runs. Job control registers __koi_jobs/fg/bg here, reached
// through the CallHandler rewrite.
func Register(name string, fn Func) {
	registry[name] = fn
}

// ShellBuiltins returns the interpreter builtins that actually work,
// for completion.
func ShellBuiltins() []string {
	return interpImplemented()
}

// Native returns the koi-native builtin names as the user types them:
// registry-internal __koi_ prefixes are stripped for display.
func Native() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, strings.TrimPrefix(name, "__koi_"))
	}
	slices.Sort(names)
	return names
}

// The two lists below used to be written out by hand here, and they went
// stale in both directions: builtins the interpreter had gained were missing
// from the "implemented" list, and `jobs` sat in the "unsupported" one long
// enough that `compgen -b` and `type jobs` disagreed about whether it existed
// (#302). They are now asked of the interpreter, which is the only place that
// knows -- so a builtin landing there shows up here without anyone
// remembering to edit this file.
//
// The unsupported names are still recognized by interp.IsBuiltin, so `type`
// calls them builtins while running one fails; listBuiltins supersedes an
// entry once a native builtin claims the name, which is what makes the
// listing describe the session rather than the build.
func interpImplemented() []string { return interp.ImplementedBuiltins() }

func interpUnsupported() []string { return interp.UnimplementedBuiltins() }

func listBuiltins(_ context.Context, hc interp.HandlerContext, _ []string) error {
	native := Native()
	// A registered koi builtin (e.g. jobs/fg/bg under job control)
	// supersedes its "unsupported" listing.
	unsup := interpUnsupported()
	unsupported := make([]string, 0, len(unsup))
	for _, name := range unsup {
		if !slices.Contains(native, name) {
			unsupported = append(unsupported, name)
		}
	}
	fmt.Fprintf(hc.Stdout, "koi builtins:\n  %s\n\n", strings.Join(native, " "))
	fmt.Fprintf(hc.Stdout, "shell builtins:\n  %s\n\n", strings.Join(interpImplemented(), " "))
	fmt.Fprintf(hc.Stdout, "recognized but not yet supported:\n  %s\n", strings.Join(unsupported, " "))
	return nil
}
