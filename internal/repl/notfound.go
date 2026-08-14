package repl

import (
	"context"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/builtins"
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
			fmt.Fprintf(hc.Stderr, "gish: command not found: %s", args[0])
			if runner := getRunner(); runner != nil {
				if s := suggestCommand(args[0], runner); s != "" {
					fmt.Fprintf(hc.Stderr, " — did you mean %q?", s)
				}
			}
			fmt.Fprintln(hc.Stderr)
			return interp.ExitStatus(127)
		}
	}
}

// suggestCommand returns the closest known command within edit
// distance 2, preferring closer, then shorter, then lexicographic.
func suggestCommand(miss string, runner *interp.Runner) string {
	extra := builtins.ShellBuiltins()
	extra = append(extra, builtins.Native()...)
	extra = append(extra, "zi")
	for name := range runner.Funcs {
		extra = append(extra, name)
	}
	best, bestDist := "", 3
	for _, c := range complete.Commands("", pathVar(runner), extra) {
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
