//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// Where a runtime diagnostic says it came from (#571).
//
// Differential, and it has to be: the prefix is `source: line N: `, and
// which source and which N is the whole question — a function names the
// file it was *defined* in, an eval'd string is numbered as if spliced
// in where it stands, and a sourced file starts again at one. Every one
// of those was measured off bash rather than reasoned about, so the test
// asks bash rather than repeating the measurement as a literal.
//
// The script is written into the case's own temp directory and run by a
// path relative to it, so both shells name the same file: bash prints
// the path as *written*, not as resolved.
const diagScript = `nosuch_top
topfn() { nosuch_in_topfn; }
topfn
. ./lib.sh
libfn
(nosuch_subshell)
eval "echo one=$LINENO
nosuch_in_eval"
eval "eval 'nosuch_nested'"
cd /nosuchdir_for_this_test
echo "end=$LINENO"
`

const diagLib = `libfn() { nosuch_in_libfn; }
nosuch_at_lib_top
`

func TestDiagnosticsSayWhereTheyCameFrom(t *testing.T) {
	if testing.Short() {
		t.Skip("differential diagnostic locations skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	tmp := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("main.sh", diagScript)
	write("lib.sh", diagLib)

	// cd into the directory first, so both shells are handed the same
	// relative path and print it the same way: bash names a script as it
	// was written, not as it resolves.
	script := "cd " + tmp + "\n. ./main.sh 2>&1"

	r := compat.Run(context.Background(), bash, koi, compat.Case{
		Name: "diagnostic locations", Script: script,
	})
	if !r.Pass {
		t.Errorf("diagnostic locations differ from bash (%s)\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
			r.Reason, r.BashOut, r.BashCode, r.KoiOut, r.KoiCode)
	}
	// Both shells agreeing on an empty answer would pass while proving
	// nothing, and so would output with no locations in it.
	if !strings.Contains(r.BashOut, "./lib.sh: line 1:") {
		t.Errorf("the oracle produced no located diagnostic, so this case cannot detect a missing one: %q", r.BashOut)
	}
}

// A command string has no file to name, so it carries no location — koi
// says the message and nothing else, where bash prints its own $0 and a
// line. That divergence is deliberate (#120 keeps $0 as `koi`), so it is
// asserted rather than compared.
func TestCommandStringDiagnosticsCarryNoLocation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	koi := buildKoi(t)
	env := hermeticEnv(t)

	stdout, stderr, code := runC(t, koi, env, "-c", "nosuch_command_string")
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if stderr != "nosuch_command_string: command not found\n" {
		t.Errorf("stderr = %q, want the bare message", stderr)
	}
	if code != 127 {
		t.Errorf("exit = %d, want 127", code)
	}
}

// The word-expansion family, which #571 missed and #584 fixed: these
// print from `expandErr` rather than through `errf`, so they carried no
// location at all while every other runtime diagnostic did.
//
// Same shapes as above, because the question is the same one: which file
// and which line, through a function, a sourced file and an eval. The
// unbound variable is last on purpose — nounset is fatal in bash, so
// anything after it would not run in either shell and the case would
// silently stop testing.
//
// An `eval` case is deliberately absent: bash quotes the *whole* eval
// string as the offending expansion where koi names only the expansion,
// and both discard the string, so the divergence is wording rather than
// behavior and is recorded on #585 instead of normalized away here.
const diagExpandScript = `echo ${${bad_at_top}}
expfn() { echo ${${bad_in_fn}}; }
expfn
. ./explib.sh
explibfn
shopt -s failglob
echo /nosuch_dir_for_koi_584*/x
set -u
echo $nope_unbound
echo unreachable
`

const diagExpandLib = `explibfn() { echo ${${bad_in_libfn}}; }
echo ${${bad_at_lib_top}}
`

func TestExpansionDiagnosticsSayWhereTheyCameFrom(t *testing.T) {
	if testing.Short() {
		t.Skip("differential diagnostic locations skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	tmp := t.TempDir()
	for name, body := range map[string]string{
		"expmain.sh": diagExpandScript,
		"explib.sh":  diagExpandLib,
	} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Run the file rather than sourcing it through compat.Run's `-c`,
	// for two reasons. bash names a script as it was *written*, so both
	// shells have to be handed the same relative path — hence the run
	// from inside tmp. And a bad substitution abandons its input unit,
	// which in a sourced file costs koi the rest of the file today
	// (#585): sourcing would measure that bug instead of this one.
	bashOut, bashCode := runInDir(t, tmp, bash, "./expmain.sh")
	koiOut, koiCode := runInDir(t, tmp, koi, "./expmain.sh")
	if bashOut != koiOut || bashCode != koiCode {
		t.Errorf("expansion diagnostics differ from bash\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
			bashOut, bashCode, koiOut, koiCode)
	}
	// The whole point is the prefix, so a run with none in it proves
	// nothing — and one that never reached the sourced file proves less.
	for _, want := range []string{"./explib.sh: line 1:", "unbound variable"} {
		if !strings.Contains(bashOut, want) {
			t.Errorf("the oracle's output lacks %q, so this case cannot detect a regression: %q",
				want, bashOut)
		}
	}
}

// Malformed arithmetic, which bash refuses while *evaluating* because
// it parses an expression from a string when it runs one (#600). koi
// parses ahead, so a script is the only shape that can show the whole
// answer: which commands ran, what they answered, and which lines the
// diagnostics named.
//
// The prose is deliberately not compared — bash quotes the expression as
// written and names the token it stopped at, which is #598 — so the
// script sends its diagnostics to a file and the test compares the
// *line numbers* out of it. That is the behavioral claim: the same
// lines failed, in the same order, and everything between them ran.
//
// Both halves of #597's split are here. A word abandons its input unit,
// so `lost1` never prints while the next line does; `(( ))` and a
// C-style loop's header are commands whose evaluation failed, so they
// answer 1 and the rest of the line runs.
const diagBadArithmScript = `exec 2>errs.txt
echo start
echo $(( 4 ? : 3 )); echo lost1
echo after1
echo $(( 1 ? 20 )); echo lost2
(( 4 + )); echo same1=$?
(( -- )); echo same2=$?
for (( i=1; i < 4; 7++ )); do n=$((n+1)); done; echo "same3=$? n=$n"
set -- a b
echo "${#:%}"; echo lost3
v=abcdef
echo "${v:1:%}"; echo lost4
echo end
`

func TestBadArithmeticIsARuntimeError(t *testing.T) {
	if testing.Short() {
		t.Skip("differential arithmetic behavior skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	// A directory per shell, since both write their diagnostics to the
	// same file name beside the script.
	dirs := make(map[string]string, 2)
	for _, shell := range []string{bash, koi} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "badarith.sh"), []byte(diagBadArithmScript), 0o600); err != nil {
			t.Fatal(err)
		}
		dirs[shell] = dir
	}
	bashOut, bashCode := runInDir(t, dirs[bash], bash, "./badarith.sh")
	koiOut, koiCode := runInDir(t, dirs[koi], koi, "./badarith.sh")
	if bashOut != koiOut || bashCode != koiCode {
		t.Errorf("what ran differs from bash\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
			bashOut, bashCode, koiOut, koiCode)
	}
	// A shell that ran nothing at all would agree on an empty answer.
	if !strings.Contains(bashOut, "end\n") || strings.Contains(bashOut, "lost1") {
		t.Fatalf("the oracle did not behave as this case assumes: %q", bashOut)
	}

	diagLines := func(shell string) []string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(dirs[shell], "errs.txt"))
		if err != nil {
			t.Fatalf("reading %s's diagnostics: %v", shell, err)
		}
		var lines []string
		for line := range strings.SplitSeq(strings.TrimSuffix(string(body), "\n"), "\n") {
			prefix, _, ok := strings.Cut(line, ": ")
			if !ok {
				t.Fatalf("%s printed an unlocated diagnostic: %q", shell, line)
			}
			num, _, ok := strings.Cut(line[len(prefix)+2:], ": ")
			if !ok {
				t.Fatalf("%s printed an unlocated diagnostic: %q", shell, line)
			}
			lines = append(lines, prefix+" "+num)
		}
		return lines
	}
	bashAt, koiAt := diagLines(bash), diagLines(koi)
	if !slices.Equal(bashAt, koiAt) {
		t.Errorf("the lines reported differ from bash\n  bash: %q\n  koi: %q", bashAt, koiAt)
	}
	if len(bashAt) != 7 {
		t.Errorf("the oracle reported %d diagnostics, want 7: %q", len(bashAt), bashAt)
	}
}

// runInDir runs a shell on a script from a given directory, with a
// scratch HOME so nothing reads or writes the developer's.
func runInDir(t *testing.T, dir, shell string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), shell, args...)
	cmd.Dir = dir
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
