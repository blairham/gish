package main

import (
	"context"
	"testing"

	"github.com/blairham/gish/internal/compat"
)

// printf against real bash (#55).
//
// The interpreter's printf rejected every precision ("invalid format
// char: .") and had no %f %e %g %X %q at all — seven of the fourteen
// forms shell scripts actually use. `%.2f` is how a script prints a
// number and `%q` is how it quotes one safely, so this was not an exotic
// corner: it was printf not being printf.
//
// Differential, because printf is exactly the builtin where writing down
// what we believe the rules are would encode the same misunderstandings
// that produce a wrong implementation. bash on the running machine says
// what the answer is.
//
// Arguments are literal in each script rather than positional, which is
// deliberate: the first version of this passed them as `-c script _ arg`
// and every case came back empty, which looked exactly like a broken
// printf. It was a separate bug in -c's positional handling (#56). A
// harness that can fail for a reason unrelated to its subject will send
// you to the wrong file.
var printfCases = []string{
	// Precision — the whole class that did not parse.
	`printf '%.2f\n' 3.14159`,
	`printf '%.0f\n' 2.5`,
	`printf '%5.1f\n' 2.71`,
	`printf '%05.2f\n' 3.14159`,
	`printf '%.3s\n' abcdef`,
	`printf '%-10.3s|\n' abcdef`,

	// Conversions the substrate did not have.
	`printf '%f\n' 3.5`,
	`printf '%e\n' 31400`,
	`printf '%E\n' 31400`,
	`printf '%g\n' 0.0001`,
	`printf '%X\n' 255`,

	// %q: the quoting round trip.
	`printf '%q\n' 'a b'`,
	`printf '%q\n' plain`,
	`printf '%q\n' ''`,
	`printf '%q\n' 'a$b'`,
	`printf '%q\n' 'x*y'`,

	// Forms that already worked, so the replacement does not regress.
	`printf '%s\n' ok`,
	`printf '%d\n' 7`,
	`printf '%i\n' 42`,
	`printf '%o\n' 8`,
	`printf '%x\n' 255`,
	`printf '%c\n' abc`,
	`printf '%b\n' 'a\tb'`,
	`printf '%%\n'`,
	`printf '%5d|\n' 42`,
	`printf '%-5d|\n' 42`,
	`printf '%+d\n' 5`,
	`printf 'plain text\n'`,

	// POSIX format reuse: the format repeats until arguments run out.
	`printf '%s-%s\n' a b c d`,
	`printf '[%s]' one two three; echo`,

	// A missing argument is an empty string or a zero, not an error.
	`printf '[%s][%d]\n' onlyone`,

	// Escapes in the format.
	`printf 'a\tb\n'`,
	`printf 'x\\y\n'`,
	`printf 'oct:\101\n'`,
	`printf 'hex:\x41\n'`,

	// Width and precision taken from the arguments.
	`printf '%*d|\n' 5 42`,
	`printf '%.*f\n' 2 3.14159`,
}

func TestPrintfMatchesBash(t *testing.T) {
	if testing.Short() {
		t.Skip("printf differential skipped in -short")
	}
	gish := buildGish(t)
	bash := requireBash(t)

	for _, script := range printfCases {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			r := compat.Run(context.Background(), bash, gish, compat.Case{
				Name: script, Script: script,
			})
			if !r.Pass {
				t.Errorf("%s\n  bash: %q (exit %d)\n  gish: %q (exit %d)",
					r.Reason, r.BashOut, r.BashCode, r.GishOut, r.GishCode)
			}
		})
	}
}
