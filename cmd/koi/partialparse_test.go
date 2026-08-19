//go:build unix

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// bash reads a script a statement at a time (#276): a syntax error on
// line 129 is reported at 129 with the first 128 lines already run and
// their side effects standing. koi parsed the whole file up front, so
// one unreadable construct anywhere discarded all of it — a user's rc
// losing its last line lost the whole rc instead.
//
// bash is the oracle for *stdout and status*, not for the message. The
// diagnostic keeps koi's own shape (#120), so comparing prose would be a
// lie that then has to be maintained against a shell we do not control;
// what a caller acts on is what ran and what the status was.
func TestPartialParseMatchesBash(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash: the oracle is unavailable")
	}
	koiBin := buildKoi(t)

	cases := []struct {
		name string
		// script is written to a file and run as an operand; the same
		// text also goes through -c, since bash reads both the same way.
		script string
		// wantErrOutput is whether a diagnostic is expected at all. An
		// `exit` in the readable prefix means bash never read far enough
		// to find the error, so there is nothing to report.
		wantErrOutput bool
	}{
		{
			name:          "the readable prefix runs",
			script:        "echo one\necho two\nif then fi\necho never\n",
			wantErrOutput: true,
		},
		{
			name: "an error inside a compound discards the whole compound",
			// The `if` never completes, so it was never a statement bash
			// could have run — but the echo before it was.
			script:        "echo before\nif true; then\n  if then fi\nfi\n",
			wantErrOutput: true,
		},
		{
			name: "the cut is by line, not by statement",
			// bash's reading unit is the line: it cannot finish reading
			// this one, so neither command on it runs.
			script:        "echo a; if then fi\necho b\n",
			wantErrOutput: true,
		},
		{
			name: "exit in the prefix wins over the error",
			// bash stopped reading at the exit, so the error further down
			// is never reached and never reported.
			script:        "echo one\nexit 7\nif then fi\n",
			wantErrOutput: false,
		},
		{
			name:          "an error on the first line runs nothing",
			script:        "if then fi\necho never\n",
			wantErrOutput: true,
		},
		{
			name:          "a clean script is untouched",
			script:        "echo one\necho two\n",
			wantErrOutput: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("script file", func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "s.sh")
				if werr := os.WriteFile(path, []byte(tc.script), 0o600); werr != nil {
					t.Fatal(werr)
				}
				compareShells(t, bashBin, koiBin, []string{path}, tc.wantErrOutput)
			})
			t.Run("-c", func(t *testing.T) {
				compareShells(t, bashBin, koiBin, []string{"-c", tc.script}, tc.wantErrOutput)
			})
		})
	}
}

// The point of running the prefix is that its side effects stand. stdout
// would be satisfied by output alone, so this checks a file.
func TestPartialParseSideEffectsStand(t *testing.T) {
	koiBin := buildKoi(t)
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness")
	script := filepath.Join(dir, "s.sh")
	body := "printf x > " + witness + "\nif then fi\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, code := run3(t, koiBin, []string{script})
	if code != 2 {
		t.Errorf("exit status = %d, want 2", code)
	}
	if _, err := os.Stat(witness); err != nil {
		t.Errorf("the prefix's side effect did not stand: %v", err)
	}
}

// `eval "$(tool init)"` is the shape that made this worth fixing: one
// construct koi cannot read at the bottom of a generated hook used to
// discard the whole hook, so the tool appeared to install and did
// nothing. `source` has the same shape for an rc file.
func TestPartialParseInEvalAndSource(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash: the oracle is unavailable")
	}
	koiBin := buildKoi(t)
	dir := t.TempDir()

	lib := filepath.Join(dir, "lib.sh")
	if werr := os.WriteFile(lib, []byte("echo sourced\nif then fi\n"), 0o600); werr != nil {
		t.Fatal(werr)
	}

	cases := []struct {
		name   string
		script string
	}{
		{"eval", "eval 'echo evaled\nif then fi'\necho ran=$?\n"},
		{"source", ". " + lib + "\necho ran=$?\n"},
	}
	statusOracle := evalStatusOracle(t, bashBin)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Both keep running afterwards — a failed eval or source is a
			// failed command, not the end of the script — so what each
			// printed *before* the error is the version-independent part,
			// and it is compared against whatever bash is here.
			wantOut, _, _ := run3(t, bashBin, []string{"-c", tc.script})
			gotOut, gotErr, _ := run3(t, koiBin, []string{"-c", tc.script})
			wantFirst, _, _ := strings.Cut(wantOut, "\n")
			gotFirst, _, _ := strings.Cut(gotOut, "\n")
			if gotFirst != wantFirst {
				t.Errorf("the readable prefix printed %q, bash printed %q", gotFirst, wantFirst)
			}
			if strings.TrimSpace(gotErr) == "" {
				t.Error("reported a syntax error with no message")
			}
			// The status is where the oracle stops being one machine's
			// bash. See evalStatusOracle.
			if statusOracle == 0 {
				t.Logf("bash here answers 1 for this; not comparing the status")
				return
			}
			want := "ran=" + strconv.Itoa(statusOracle) + "\n"
			if !strings.HasSuffix(gotOut, want) {
				t.Errorf("output %q does not end in %q", gotOut, want)
			}
		})
	}
}

// evalStatusOracle reports the status this machine's bash gives `eval` and
// `source` when what they were handed does not parse, or 0 when it is a
// bash whose answer koi deliberately does not match.
//
// bash changed this in **5.3**: 5.2 and earlier answer 1, 5.3 answers 2 —
// which is also what every version answers for the same error in a script
// or in `-c`, so 5.3 made the family consistent. koi answers 2 throughout,
// because bash 5.3 is the version this project pins its claim to: the
// suite is bash 5.3's, the differential oracle #278 builds is 5.3, and
// docs/bash-suite.md says so on its first line.
//
// This is #271's situation rather than a bug — the oracle is not the same
// on every machine — and it is handled the same way: narrowly, in the open,
// and only for the assertion that actually differs. Both CI runners
// answered 1 when this was written (macos-latest ships 3.2.57), so both
// take the skip while the prefix comparison above still runs everywhere.
// It is asked of the oracle rather than derived from a version string, so
// a runner image that moves to 5.3 starts checking the status by itself.
func evalStatusOracle(t *testing.T, bashBin string) int {
	t.Helper()
	out, _, _ := run3(t, bashBin, []string{"-c", "eval 'if then fi' 2>/dev/null\necho $?\n"})
	switch strings.TrimSpace(out) {
	case "2":
		return 2
	case "1":
		return 0
	default:
		t.Fatalf("bash answered %q for a failed eval, which is neither 1 nor 2", strings.TrimSpace(out))
		return 0
	}
}

// compareShells runs argv through both shells and requires the same
// stdout and the same exit status. stderr is compared only for presence,
// since a syntax error that says nothing leaves the status as the only
// clue anyone gets.
func compareShells(t *testing.T, bashBin, koiBin string, argv []string, wantErrOutput bool) {
	t.Helper()

	wantOut, _, wantCode := run3(t, bashBin, argv)
	gotOut, gotErr, gotCode := run3(t, koiBin, argv)

	if gotOut != wantOut {
		t.Errorf("stdout = %q, bash = %q", gotOut, wantOut)
	}
	if gotCode != wantCode {
		t.Errorf("exit status = %d, bash = %d (stderr %q)", gotCode, wantCode, gotErr)
	}
	if wantErrOutput && strings.TrimSpace(gotErr) == "" {
		t.Error("reported a syntax error with no message")
	}
	if !wantErrOutput && strings.TrimSpace(gotErr) != "" {
		t.Errorf("stderr = %q, want nothing", gotErr)
	}
}

// run3 keeps stdout and stderr apart, which runArgv does not: the two
// shells' diagnostics differ by design, so only stdout can be compared.
func run3(t *testing.T, bin string, argv []string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, argv...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		t.Fatalf("running %s: %v", bin, err)
	}
	return out.String(), errb.String(), code
}
