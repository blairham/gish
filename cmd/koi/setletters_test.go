//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runScriptArgv runs a whole argv with stdout kept apart from stderr,
// in a scratch HOME. Separate streams matter here: koi reports an
// option it cannot honor while bash accepts it silently, and folding
// the two together would turn that stated divergence into a failure.
func runScriptArgv(t *testing.T, bin string, argv []string) (string, int) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(bin, argv...)
	cmd.Dir = home
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "HOME=" + home}
	out, err := cmd.Output()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", bin, err)
	}
	return string(out), code
}

// bash takes any `set` option in argv, and so must koi (#426).
//
// `bash -euxc 'cmd'` is what CI files, Makefiles and test harnesses
// write. koi answered `unknown option "u" in "-uc"` and a usage dump,
// exit 2 — so anything pointing $SHELL at koi hit a wall, and against
// the bash suite one such invocation produced hundreds of diff lines of
// repeated usage text.
//
// stdout and exit status are compared against bash. stderr is not: koi
// reports an option it does not implement rather than accepting it
// silently (see the -o verbose case below), which is a deliberate
// divergence in the direction of saying so.
func TestSetOptionLettersInArgvMatchBash(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	cases := []struct {
		name string
		argv []string
	}{
		{"a set letter clustered with -c", []string{"-uc", "echo hi"}},
		{"several letters at once", []string{"-euxc", "echo hi"}},
		{"letters in their own argument", []string{"-u", "-c", "echo hi"}},
		{"the plus form turns one off", []string{"+u", "-c", "echo hi"}},
		{"koi's own flags still cluster", []string{"-lc", "echo hi"}},
		{"a set letter mixed with koi's", []string{"-ixc", "echo hi"}},
		{"noglob actually takes effect", []string{"-fc", "echo ./*"}},
		{"nounset actually takes effect", []string{"-uc", "echo ${nope:-fallback}"}},
		{"errexit actually takes effect", []string{"-ec", "false; echo unreachable"}},
		{"an unknown letter is still a usage error", []string{"-Zc", "echo hi"}},
		{"an unknown letter in a plus cluster too", []string{"+Z", "-c", "echo hi"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantOut, wantCode := runScriptArgv(t, bashBin, tc.argv)
			gotOut, gotCode := runScriptArgv(t, koiBin, tc.argv)
			if gotOut != wantOut {
				t.Errorf("stdout = %q, bash = %q", gotOut, wantOut)
			}
			if gotCode != wantCode {
				t.Errorf("exit status = %d, bash = %d", gotCode, wantCode)
			}
		})
	}
}

// `-o name` is the long spelling and reaches the interpreter the same
// way. koi differs from bash on purpose for an option it does not
// implement: bash accepts it silently, koi runs the command and says on
// stderr that it could not turn the option on. Saying so is the point —
// the alternative is a shell that claims a mode it is not in.
//
// The example used to be `-o posix`, which is implemented now (#395);
// `-o verbose` stands in for the same rule.
func TestSetOptionLongFormInArgv(t *testing.T) {
	koiBin := buildKoi(t)

	out, code := runScriptArgv(t, koiBin, []string{"-o", "noglob", "-c", "echo ./*"})
	if code != 0 || out != "./*\n" {
		t.Errorf("-o noglob = (%q, %d), want the unexpanded glob and 0", out, code)
	}

	// An unimplemented option is refused rather than silently accepted,
	// and the refusal must not stop the command from running.
	outErr, codeErr := runArgv(t, koiBin, []string{"-o", "verbose", "-c", "echo hi"})
	if codeErr != 0 {
		t.Errorf("-o verbose exit status = %d, want 0: %q", codeErr, outErr)
	}
	if !strings.Contains(outErr, "hi") {
		t.Errorf("-o verbose did not run the command: %q", outErr)
	}
	if !strings.Contains(outErr, "verbose") {
		t.Errorf("-o verbose was accepted silently: %q", outErr)
	}
}
