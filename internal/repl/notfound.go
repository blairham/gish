package repl

import (
	"context"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/complete"
)

// notFoundMiddleware improves the shell's worst error message (#42):
// instead of a bare "command not found", suggest the nearest real
// command (edit distance ≤ 2 over PATH executables, builtins, and
// session functions). Runs only on the failure path — the command has
// already missed, latency is human-scale.
// The runner is created after the middleware chain is assembled, so it
// arrives through a late-bound accessor.
func notFoundMiddleware(getRunner func() *interp.Runner) func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			hc := interp.HandlerCtx(ctx)
			if strings.Contains(args[0], "/") {
				return next(ctx, args) // explicit paths get the real error
			}
			if _, err := interp.LookPathDir(hc.Dir, hc.Env, args[0]); err == nil {
				return next(ctx, args)
			}
			runner := getRunner()
			// The distro hook comes first and gets the whole command line
			// (#163): `command_not_found_handle` is how Debian/Ubuntu and
			// Fedora turn a miss into "the package you want is X", and
			// every one of those handlers reads "$@", not just "$1".
			if runner != nil {
				if name := commandNotFoundHandler(runner); name != "" {
					return runNotFoundHandler(ctx, runner, name, args)
				}
			}
			fmt.Fprintf(hc.Stderr, "gish: command not found: %s", args[0])
			if runner != nil {
				if s := suggestCommand(args[0], runner); s != "" {
					// Suggest the line, not the word: the user typed
					// `gti status`, and what they want back is something
					// they can act on without retyping the rest.
					fmt.Fprintf(hc.Stderr, " — did you mean %q?", strings.Join(append([]string{s}, args[1:]...), " "))
				}
			}
			fmt.Fprintln(hc.Stderr)
			return interp.ExitStatus(127)
		}
	}
}

// commandNotFoundHandler returns the name of the session's
// command-not-found hook, or "" when there is none.
//
// Both spellings are honored because both are in the wild: bash calls
// it command_not_found_handle and zsh command_not_found_handler, and a
// switcher's rc carries whichever their distro installed. Inheriting
// the ecosystem means running the file people already have, not asking
// them to rename a function.
func commandNotFoundHandler(runner *interp.Runner) string {
	for _, name := range []string{"command_not_found_handle", "command_not_found_handler"} {
		if _, ok := runner.Funcs[name]; ok {
			return name
		}
	}
	return ""
}

// runNotFoundHandler calls the hook with the full argument list.
//
// It runs in a subshell copy because the exec handler is called from
// inside the parent's own Run, and a runner is not reentrant. That is
// also the conservative reading of the hook's job: report, suggest,
// maybe install — not silently rewrite the calling shell's state.
//
// The arguments are passed as AST nodes rather than a formatted command
// string, so nothing in them is re-parsed, re-expanded, or re-globbed:
// the hook sees exactly the words the user typed.
func runNotFoundHandler(ctx context.Context, runner *interp.Runner, name string, args []string) error {
	words := make([]*syntax.Word, 0, len(args)+1)
	for _, a := range append([]string{name}, args...) {
		words = append(words, &syntax.Word{Parts: []syntax.WordPart{&syntax.SglQuoted{Value: a}}})
	}
	stmt := &syntax.Stmt{Cmd: &syntax.CallExpr{Args: words}}
	return runner.Subshell().Run(ctx, stmt)
}

// suggestCommand returns the closest known command within edit
// distance 2, preferring closer, then shorter, then lexicographic.
func suggestCommand(miss string, runner *interp.Runner) string {
	best, bestDist := "", 3
	for _, c := range complete.Commands("", pathVar(runner), sessionCommandNames(runner)) {
		name := c.Value
		if abs(len(name)-len(miss)) > 2 {
			continue
		}
		if d := editDistance(miss, name, 2); d < bestDist ||
			(d == bestDist && best != "" && (len(name) < len(best) || (len(name) == len(best) && name < best))) {
			if d <= 2 {
				best, bestDist = name, d
			}
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// editDistance is Damerau-Levenshtein (optimal string alignment) with
// an early-exit bound: adjacent transpositions cost 1, so gti→git is
// distance 1 — typos are mostly swaps, and the human answer should win.
func editDistance(a, b string, bound int) int {
	ra, rb := []rune(a), []rune(b)
	prev2 := make([]int, len(rb)+1)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
			if cur[j] < rowMin {
				rowMin = cur[j]
			}
		}
		if rowMin > bound {
			return bound + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(rb)]
}
