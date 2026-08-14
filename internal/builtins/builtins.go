// Package builtins hosts gish-native builtins, layered over the
// interpreter through mvdan's ExecHandler middleware: the handler fires
// exactly when a command name is not an interpreter builtin, which makes
// it the natural extension seam. This package is its first occupant; job
// control's jobs/fg/bg (#5) and plugin-provided commands (M3
// CommandProvider) build on the same mechanism.
//
// Name policy: a gish builtin shadows PATH binaries of the same name
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

	"mvdan.cc/sh/v3/interp"
)

// Func is a gish-native builtin. args excludes the command name; output
// goes to the handler context's stdio, never the process's.
type Func func(ctx context.Context, hc interp.HandlerContext, args []string) error

var registry = map[string]Func{}

func init() {
	// Populated here rather than in the literal: listBuiltins reaches
	// back into the registry via Native, which a literal would make an
	// initialization cycle.
	//
	// No "help" alias: bash has a help builtin, so the interpreter
	// swallows the name before this seam ever sees it.
	registry["builtins"] = listBuiltins
}

// ExecHandler is the middleware that dispatches gish-native builtins and
// passes everything else on (ultimately to the PATH-exec handler).
func ExecHandler(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if fn, ok := registry[args[0]]; ok {
			return fn(ctx, interp.HandlerCtx(ctx), args[1:])
		}
		return next(ctx, args)
	}
}

// Native returns the gish-native builtin names, sorted.
func Native() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// interpImplemented is the set of interpreter builtins that actually run,
// taken from mvdan.cc/sh/v3's interp dispatch (v3.13). `export`, `local`,
// and `readonly` are parser-level keywords that behave like builtins.
// TestInterpListsAccurate guards this list against upstream drift.
var interpImplemented = []string{
	":", "[", "alias", "break", "builtin", "cd", "command", "continue",
	"dirs", "echo", "eval", "exec", "exit", "export", "false", "getopts",
	"hash", "local", "mapfile", "popd", "printf", "pushd", "pwd", "read",
	"readarray", "readonly", "return", "set", "shift", "shopt", "source",
	"test", "trap", "true", "type", "unalias", "unset", "wait",
}

// interpUnsupported is recognized by the interpreter's IsBuiltin (so
// `type` calls them builtins) but fails with "unsupported builtin" when
// run. jobs/fg/bg arrive with job control (#5).
var interpUnsupported = []string{
	"bg", "fc", "fg", "jobs", "kill", "newgrp", "times", "umask",
}

func listBuiltins(_ context.Context, hc interp.HandlerContext, _ []string) error {
	fmt.Fprintf(hc.Stdout, "gish builtins:\n  %s\n\n", strings.Join(Native(), " "))
	fmt.Fprintf(hc.Stdout, "shell builtins:\n  %s\n\n", strings.Join(interpImplemented, " "))
	fmt.Fprintf(hc.Stdout, "recognized but not yet supported:\n  %s\n", strings.Join(interpUnsupported, " "))
	return nil
}
