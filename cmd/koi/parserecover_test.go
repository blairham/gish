//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A syntax error inside a compound array assignment, differentially
// against real bash (#581).
//
// bash reports it and keeps reading: the input *unit* being parsed is
// discarded — the line of a script, the whole string of a `-c` — the
// status is 1, and the next line runs. koi refused it at parse time and
// forfeited the rest of the file, which is what cost bash's own
// array.tests 851 lines.
//
// These cases run a script *file* rather than going through compat.Run,
// which runs `-c`: the line being the unit is the load-bearing
// measurement here and a command string is only ever one unit. The
// `-c` shape has a case of its own below.
//
// One thing is normalized, in Go rather than in the script so it is
// visible: bash prints a *second* diagnostic line quoting the offending
// source line, which koi does not have at the point the runner prints it
// (#233 keeps koi's own message shape, and #571 already gives the first
// line bash's `file: line N: ` prefix). Nothing else is touched.
var parseRecoverCases = []struct {
	name string
	body string
	// wantAbort marks a case where the script is expected to *end* at
	// the error rather than carry on, which is bash's answer for an
	// ordinary grammar error. Those are compared on status alone, since
	// the two shells word a fatal parse error differently by decision.
	wantAbort bool
}{
	{
		// The unit is the line: `echo two` sits before the bad
		// assignment and does not run, while the next line does.
		name: "line is the unit",
		body: "echo one\n" +
			"echo two; x=(a & b); echo \"same=$?\"\n" +
			"echo tail\n",
	},
	{
		// A one-line compound command is discarded whole, so the loop
		// body never runs at all.
		name: "a compound command on one line",
		body: "for i in 1 2; do x=(a & b); echo \"in=$?\"; done\n" +
			"echo \"after=$?\"\n",
	},
	{
		// Every token measured, each naming itself. The nested `(` is
		// the one that needs the skip to count depth rather than
		// stopping at the first `)` it sees.
		name: "each unexpected token names itself",
		body: "x=(a & b)\n" +
			"x=(a && b)\n" +
			"x=(a | b)\n" +
			"x=(a || b)\n" +
			"x=(a ; b)\n" +
			"x=(a > b)\n" +
			"x=(a (b) c)\n" +
			"declare y=(p & q)\n" +
			"z+=(p & q)\n" +
			"echo \"tail=$?\"\n",
	},
	{
		// The status a discarded line leaves is 1, and it is readable
		// from the next line rather than only from the shell's own exit.
		name: "the discarded line leaves status 1",
		body: "x=(a & b)\n" +
			"echo \"after=$?\"\n" +
			"true\n" +
			"echo \"clean=$?\"\n",
	},
	{
		// A name the shell has never seen recovers the same way, so the
		// recovery is not keyed on the variable already existing.
		//
		// `a[2]=(x & y)` — a *subscripted* target — is deliberately not
		// here: koi refuses that at parse time with "arrays cannot be
		// nested" because it has no answer for assigning a list to an
		// array member at all, which bash reports at runtime as
		// `a[2]: cannot assign list to array member`. That is a
		// different gap and is filed rather than half-fixed here.
		name: "on a name never seen before",
		body: "brandnew=(p ; q)\n" +
			"echo \"fresh=$?\"\n" +
			"echo \"len=${#brandnew[@]}\"\n",
	},
	{
		// An ordinary grammar error is *not* recoverable, and that half
		// is what keeps this honest: bash ends a non-interactive shell
		// for these, so koi must too. Only "did it stop" is compared —
		// the wording differs by decision (#233), and so does the
		// status, since bash answers 2 for a grammar error and 1 for an
		// unterminated compound assignment where koi answers 2 for both
		// (filed, not fixed here).
		name:      "an unexpected paren ends the script",
		body:      "echo one\necho )\necho tail\n",
		wantAbort: true,
	},
	{
		name:      "an unterminated array ends the script",
		body:      "echo one\nx=(one two\n",
		wantAbort: true,
	},
}

// assertStopped checks that a shell ended the script at the error
// rather than reading past it: a non-zero status, and no sign of the
// line after it. The status alone would pass for a shell that ran the
// rest and happened to fail.
func assertStopped(t *testing.T, name, shell, out string, code int) {
	t.Helper()
	if code == 0 {
		t.Errorf("%s: %s answered 0 for a fatal parse error: %q", name, shell, out)
	}
	if strings.Contains(out, "tail") {
		t.Errorf("%s: %s read past a fatal parse error: %q", name, shell, out)
	}
}

// bashQuotedLine matches the second line bash prints for a syntax error,
// which quotes the source line it could not read.
var bashQuotedLine = regexp.MustCompile("(?m)^.*: line [0-9]+: `.*\n")

func TestArraySyntaxErrorCostsTheUnit(t *testing.T) {
	if testing.Short() {
		t.Skip("differential parse recovery skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	for _, tc := range parseRecoverCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "s.sh")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			bashOut, bashCode := runScriptFile(t, bash, path)
			koiOut, koiCode := runScriptFile(t, koi, path)
			bashOut = bashQuotedLine.ReplaceAllString(bashOut, "")
			if tc.wantAbort {
				assertStopped(t, tc.name, "bash", bashOut, bashCode)
				assertStopped(t, tc.name, "koi", koiOut, koiCode)
				return
			}
			if bashCode != koiCode {
				t.Errorf("%s: exit status differs: bash %d, koi %d\n  bash: %q\n  koi: %q",
					tc.name, bashCode, koiCode, bashOut, koiOut)
			}
			if bashOut != koiOut {
				t.Errorf("%s: output differs\n  bash: %q\n  koi: %q", tc.name, bashOut, koiOut)
			}
			// Agreeing on silence would pass every case here.
			if !strings.Contains(bashOut, "syntax error") {
				t.Errorf("%s: the oracle reported no syntax error, so the case proves nothing: %q",
					tc.name, bashOut)
			}
		})
	}
}

// TestRecoveredLineRunsNothingOfItself is the assertion the differential
// cases cannot make: the *side effects* of the discarded line are gone,
// not merely its output.
//
// A line that prints nothing and writes a file would pass every case
// above while still having run, which is the shape of an incomplete fix.
func TestRecoveredLineRunsNothingOfItself(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test skipped in -short")
	}
	koi := buildKoi(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	path := filepath.Join(dir, "s.sh")
	body := "touch " + marker + "; x=(a & b)\n" +
		"test -e " + marker + " && echo touched\n" +
		"echo \"x=[${x[*]}] len=${#x[@]}\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := runScriptFile(t, koi, path)
	if strings.Contains(out, "touched") {
		t.Errorf("the touch before the bad assignment ran, so the line was not discarded:\n%s", out)
	}
	if !strings.Contains(out, "x=[] len=0") {
		t.Errorf("the discarded assignment left something behind: %q", out)
	}
}

// TestRecoveredCommandStringIsOneUnit covers the `-c` shape, where the
// unit is the whole string: nothing runs, and the shell exits 1 rather
// than the 2 a fatal parse error answers.
func TestRecoveredCommandStringIsOneUnit(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)
	const script = `x=(a & b); echo unreachable`
	for _, shell := range []string{bash, koi} {
		out, code := runShellScoped(t, shell, "-c", script)
		out = bashQuotedLine.ReplaceAllString(out, "")
		if code != 1 {
			t.Errorf("%s -c: exit %d, want 1 (%q)", filepath.Base(shell), code, out)
		}
		if strings.Contains(out, "unreachable") {
			t.Errorf("%s -c: ran the rest of the string: %q", filepath.Base(shell), out)
		}
		if !strings.Contains(out, "syntax error near unexpected token") {
			t.Errorf("%s -c: no diagnostic: %q", filepath.Base(shell), out)
		}
	}
}

// runScriptFile runs a script file the way a caller would.
//
// It is not runHermetic next door: that one is koi-only and always
// passes `-c`, and both halves are wrong here — the oracle is bash, and
// a command string is exactly the shape these cases are not about.
func runScriptFile(t *testing.T, shell, path string) (string, int) {
	t.Helper()
	return runShellScoped(t, shell, path)
}

func runShellScoped(t *testing.T, shell string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), shell, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"LC_ALL=C", "TERM=dumb", "KOI_WELCOME=off",
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	case err != nil:
		t.Fatalf("running %s: %v", shell, err)
	}
	return string(out), 0
}
