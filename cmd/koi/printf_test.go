//go:build unix

package main

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
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
	koi := buildKoi(t)
	bash := requireBash(t)

	for _, script := range printfCases {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			r := compat.Run(context.Background(), bash, koi, compat.Case{
				Name: script, Script: script,
			})
			if !r.Pass {
				t.Errorf("%s\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
					r.Reason, r.BashOut, r.BashCode, r.KoiOut, r.KoiCode)
			}
		})
	}
}

// Invalid numeric arguments (#222).
//
// These are separate from printfCases because they need a bash 4 oracle
// and a normalized comparison, and because what they check is the
// *diagnostic*, which the rest of the printf corpus never produces.
//
// The gap they close: a `%d` given a non-number used to print 0 and
// exit 0, so `printf "%d" "$n" || die` never fired and a wrong number
// flowed onward with nothing to indicate it.
var printfNumberCases = []string{
	// The reported cases.
	`printf '%d\n' notanum`,
	`printf '%f\n' notanum`,
	`printf '%x\n' zz`,
	`printf '%d\n' " "`,

	// A value still comes back: bash substitutes what it managed to
	// read, so these are a complaint *and* a number.
	`printf '%d\n' 12abc`,
	`printf '%d\n' "42 "`,
	`printf '%f\n' 3.5x`,

	// Leading whitespace is fine; only what follows the digits is not.
	`printf '%d\n' " 42"`,

	// C's integer grammar, not Go's: base-0 ParseInt would read 1_000
	// as 1000, silently computing a different number than bash.
	`printf '%d\n' 1_000`,
	`printf '%d\n' 0x10`,
	`printf '%d\n' 010`,
	`printf '%d\n' "+5"`,
	`printf '%x\n' -1`,

	// POSIX's character-value form, which is not an error.
	`printf '%d\n' "'a"`,
	`printf '%d\n' "'ab"`,
	`printf '%f\n' "'a"`,

	// inf and nan are ordinary parsable input, not complaints.
	`printf '%f\n' nan`,
	`printf '%f\n' inf`,

	// One complaint per bad argument, and the format keeps running.
	`printf '%d %d\n' bad1 bad2`,
	`printf '%d\n' 1 2 x`,

	// Ordering: bash writes the complaint as it reads the argument,
	// before the formatted line is flushed. Combined output catches it.
	`printf '%s|%d\n' ok bad`,

	// A *missing* argument is not a bad one — no complaint, status 0.
	`printf '%d %d\n' 5`,

	// The assignment form carries the same rules: the partial value is
	// stored and the status is still 1.
	`printf -v x '%d' notanum; echo "rc=$? x=[$x]"`,
}

// Deliberately absent from the list above, because bash does not agree
// with itself about them across platforms. Each was measured on macOS
// bash 5.3 (BSD libc), Ubuntu bash 5.2.21 (glibc) and Alpine bash 5.2/5.3
// (musl) before being excluded; the cases that *are* listed came back
// byte-identical on all of them.
//
//   printf '%d' ""        bash 5.2 is silent and exits 0; 5.3 complains
//                         and exits 1. A version change, not a libc one
//                         (musl 5.2 and 5.3 differ from each other).
//                         koi follows 5.3.
//
//   printf '%d' 0b101     glibc's strtol takes binary literals, so bash
//                         on Ubuntu answers 5 with no complaint, while
//                         musl and BSD answer 0 and complain. POSIX has
//                         no binary form; koi follows POSIX.
//
//   overflow, 1e400       the message is strerror(ERANGE) verbatim, and
//                         all three libcs word it differently ("Result
//                         too large" / "Numerical result out of range" /
//                         "Result not representable"); the status is 0
//                         on 5.2 and 1 on 5.3. And %f of 1e400 is `inf`
//                         only where long double is 64-bit — everywhere
//                         else bash prints 400 digits, which Go's
//                         float64 cannot represent at all.
//
// koi has one behavior on every platform, so exact parity here is not
// available at any price. What it does instead is pinned by unit tests
// in internal/builtins (TestParseIntFollowsC and friends), which need no
// oracle and therefore run everywhere.

func TestPrintfInvalidNumbersMatchBash(t *testing.T) {
	if testing.Short() {
		t.Skip("printf differential skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)
	// bash 3.2 — what a stock macOS ships as /bin/bash — predates most
	// of this: it discards the partial value (`12abc` is 0, not 12),
	// says nothing at all about an empty string, and treats an overflow
	// as a *warning* with status 0. koi follows modern bash, which is
	// the interface it claims (#120), so an old oracle would assert the
	// opposite of the intended behavior.
	if major := bashMajor(t, bash); major < 4 {
		t.Skipf("oracle is bash %d: invalid-number reporting changed in bash 4", major)
	}

	for _, script := range printfNumberCases {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			koiOut, koiCode := runNormalized(t, koi, script)
			bashOut, bashCode := runNormalized(t, bash, script)
			if koiOut != bashOut || koiCode != bashCode {
				t.Errorf("koi %q (exit %d)\nbash %q (exit %d)",
					koiOut, koiCode, bashOut, bashCode)
			}
		})
	}
}

// shellPrefix matches the "bash: line 1: " a shell puts in front of its
// own diagnostics. It is stripped before comparing because koi
// deliberately does not answer to bash's name (#120) — the message
// after the prefix is the part that has to agree.
var shellPrefix = regexp.MustCompile(`(?m)^[^:\n]*: line [0-9]+: `)

func runNormalized(t *testing.T, shell, script string) (string, int) {
	t.Helper()
	out, code := runShell(t, shell, script)
	return shellPrefix.ReplaceAllString(out, ""), code
}

// bashMajor asks the oracle what it is, so a version-sensitive case can
// skip rather than assert the wrong thing.
func bashMajor(t *testing.T, bash string) int {
	t.Helper()
	out, err := exec.Command(bash, "-c", "echo ${BASH_VERSINFO[0]}").Output()
	if err != nil {
		t.Fatalf("asking %s its version: %v", bash, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("unparsable bash version %q", out)
	}
	return n
}
