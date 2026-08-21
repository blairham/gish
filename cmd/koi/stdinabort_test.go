//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An aborting error abandons the line when commands come from standard
// input, and the last command's status is the session's (#599).
//
// #469's category means *abandon this input unit and go back to reading*.
// It worked for a script file, where the unit is the line (#450), and for
// `-c`, where it is the string, and did nothing at all when the shell read
// its commands from standard input: `printf 'echo "${x:}"; echo post\n' |
// koi` printed `post`, which bash does not, and left `$?` at 0 on the next
// line. The piped loop ran statements one at a time through `Runner.Run`,
// and both rules a *reading unit* has live in the interpreter's own
// top-level loop, which only `Runner.RunStmts` reaches.
//
// This test has to live here rather than in internal/shell/interp's table:
// that table's oracle, TestRunnerRunConfirm, feeds bash on **standard
// input**, so a case there compares a run of this exact shape against a
// bash run of the same shape and cannot see a divergence in either
// direction. Two shapes are driven per case, `cat script | koi` and
// `koi < script`, because they are the two a user meets and they are one
// line apart in the code.
//
// What is compared to bash is **stdout and the exit status**, which is
// where the whole claim lives: the rest of the abandoned line never
// printed, `$?` on the next line reads 1, and `exit` still ends things.
// Diagnostic prose is not compared — bash prefixes its own `$0` and a
// line number where koi prints neither for standard input (#120, #571),
// and two of these messages have their own recorded wording divergences
// (#598). What *is* asserted about stderr is the property that keeps this
// from passing vacuously: a shell that silently swallowed the error would
// produce bash's stdout too.
func TestPipedStdinAbandonsTheAbortedLine(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	cases := []struct {
		name string
		// script is fed on standard input, both ways.
		script string
		// diagnoses is set where the case is about an error, so the run
		// must say something rather than quietly agreeing with bash.
		diagnoses bool
	}{
		{
			name:      "a bad substitution",
			script:    "echo \"${x:}\"; echo post\necho next $?\n",
			diagnoses: true,
		},
		{
			name:      "a failed glob under failglob",
			script:    "shopt -s failglob\necho nomatch-*.zzz; echo post\necho next $?\n",
			diagnoses: true,
		},
		{
			name:      "a bad array subscript",
			script:    "a=(1 2 3)\nb[]=x; echo post\necho next $?\n",
			diagnoses: true,
		},
		{
			name:      "an arithmetic assignment to a non-variable",
			script:    "echo $((5 += 2)); echo post\necho next $?\n",
			diagnoses: true,
		},
		{
			name:      "a division by zero",
			script:    "echo $((1/0)); echo post\necho next $?\n",
			diagnoses: true,
		},
		{
			name:      "an invalid indirect expansion",
			script:    "x=\"a b\"; echo \"${!x}\"; echo post\necho next $?\n",
			diagnoses: true,
		},
		{
			name:      "a readonly assignment",
			script:    "readonly r=1\nr=2; echo post\necho next $?\n",
			diagnoses: true,
		},
		{
			name:      "a nested expansion",
			script:    "echo ${${foo}}; echo post\necho next $?\n",
			diagnoses: true,
		},
		{
			// The unit is the line, so what came before the error on it
			// has already run and what follows never does.
			name:      "the statements before it on the line still ran",
			script:    "echo pre; echo \"${x:}\"; echo post\necho next $?\n",
			diagnoses: true,
		},
		{
			// A compound command is one unit however many lines it spans,
			// so the rest of the block and the remaining iterations go too.
			name:      "a compound command is abandoned whole",
			script:    "if true; then\necho in\necho \"${x:}\"\necho after\nfi\necho next $?\n",
			diagnoses: true,
		},
		{
			name:      "a loop stops iterating",
			script:    "for i in 1 2; do\necho iter $i\necho \"${x:}\"\ndone\necho next $?\n",
			diagnoses: true,
		},
		{
			// The abandonment must not be wider than the unit: an abort
			// inside a subshell ends that subshell, and the line carries on.
			name:      "a subshell keeps the rest of the line",
			script:    "( echo \"${x:}\"; echo post ); echo post2\necho next $?\n",
			diagnoses: true,
		},
		{
			name:      "a pipeline stage keeps the rest of the line",
			script:    "echo \"${x:}\" | cat; echo post\necho next $?\n",
			diagnoses: true,
		},
		{
			// Abandoning a line must not become abandoning everything:
			// `exit` still ends the session, with its own status.
			name:      "exit still ends the session",
			script:    "echo \"${x:}\"; echo post\nexit 7\necho unreached\n",
			diagnoses: true,
		},
		{
			name:      "return still ends the function",
			script:    "f() { echo a; return 3; echo b; }\nf\necho next $?\n",
			diagnoses: false,
		},
		{
			// The other half of RunStmts' contract, and the half this loop
			// never had: `printf 'false\n' | koi` answered 0, so a failing
			// script piped into the shell reported success.
			name:      "the last command's status is the session's",
			script:    "echo hi\nfalse\n",
			diagnoses: false,
		},
		{
			name:      "the EXIT trap fires when the input runs out",
			script:    "trap 'echo bye' EXIT\necho hi\n",
			diagnoses: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, shape := range []string{"piped", "redirected"} {
				t.Run(shape, func(t *testing.T) {
					wantOut, _, wantCode := runOnStdin(t, bashBin, tc.script, shape == "piped")
					gotOut, gotErr, gotCode := runOnStdin(t, koiBin, tc.script, shape == "piped")
					if gotOut != wantOut {
						t.Errorf("stdout = %q, bash = %q", gotOut, wantOut)
					}
					if gotCode != wantCode {
						t.Errorf("exit = %d, bash = %d", gotCode, wantCode)
					}
					if tc.diagnoses && strings.TrimSpace(gotErr) == "" {
						t.Errorf("nothing was reported: a silent shell prints bash's stdout too")
					}
				})
			}
		})
	}
}

// runOnStdin runs bin with script on its standard input, either through a
// pipe (`cat script | shell`) or as a redirected file (`shell < script`).
// The two are the shapes a user meets and they are not the same code path
// — the piped loop follows a redirected fd 0 only when the script *is*
// standard input (#516) — so both are driven rather than one being assumed
// from the other.
func runOnStdin(t *testing.T, bin, script string, piped bool) (stdout, stderr string, code int) {
	t.Helper()
	// A hermetic home, so no rc of the developer's is read and the
	// failglob case is looking at a directory with nothing in it.
	home := t.TempDir()
	cmd := exec.Command(bin)
	cmd.Dir = home
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "HOME=" + home, "TERM=dumb"}
	if piped {
		cmd.Stdin = strings.NewReader(script)
	} else {
		path := filepath.Join(home, "script")
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cmd.Stdin = f
	}
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", bin, err)
	}
	return out.String(), errOut.String(), code
}
