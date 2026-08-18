//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// Positional parameters on the non-interactive paths (#56).
//
// Neither path had them. `koi -c 'echo $1' _ hi` printed nothing, and
// `koi script.sh a b` left $1 empty and $# at zero — so any script
// taking arguments silently did the wrong thing rather than failing
// loudly, which is the worst way for a shell to be wrong.
//
// Found sideways: the builtin matrix's first harness passed its
// arguments positionally and every case came back empty, which looked
// exactly like a broken printf. A harness that can fail for a reason
// unrelated to its subject sends you to the wrong file, so these have
// their own tests rather than living as an assumption inside another
// suite.

// paramCases run under both shells; bash is the oracle for what $0, $1,
// $# and $* should be.
var paramCases = []struct {
	name, script string
	args         []string
}{
	{
		name:   "-c gives the first operand to $0",
		script: `echo "0=[$0] 1=[$1] 2=[$2] n=[$#]"`,
		args:   []string{"myname", "a", "b"},
	},
	{
		name:   "-c with only a name has no parameters",
		script: `echo "0=[$0] n=[$#]"`,
		args:   []string{"solo"},
	},
	{
		name:   "-c with no operands at all",
		script: `echo "n=[$#]"`,
	},
	{
		// The `--` guard: without it a leading-dash parameter is read as
		// a shell option, so `-v` would try to set -v instead of arriving
		// as $1.
		name:   "dash-prefixed parameters are values, not options",
		script: `echo "1=[$1] 2=[$2] n=[$#]"`,
		args:   []string{"_", "-v", "--flag"},
	},
	{
		name:   "$* and $@ see everything",
		script: `echo "star=[$*]"; for a in "$@"; do echo "arg=[$a]"; done`,
		args:   []string{"_", "one", "two three"},
	},
	{
		name:   "shift walks the parameters",
		script: `shift; echo "1=[$1] n=[$#]"`,
		args:   []string{"_", "a", "b", "c"},
	},
}

func TestDashCPassesPositionalParameters(t *testing.T) {
	if testing.Short() {
		t.Skip("differential skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	for _, tc := range paramCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			koiOut, koiCode := runShellArgs(t, koi, tc.script, tc.args)
			bashOut, bashCode := runShellArgs(t, bash, tc.script, tc.args)
			if koiOut != bashOut || koiCode != bashCode {
				t.Errorf("differs from bash\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
					bashOut, bashCode, koiOut, koiCode)
			}
		})
	}
}

// A script file takes its parameters from everything after the path,
// with $0 being the path itself.
func TestScriptFilePassesPositionalParameters(t *testing.T) {
	if testing.Short() {
		t.Skip("differential skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "s.sh")
	if err := os.WriteFile(path, []byte(`echo "1=[$1] 2=[$2] n=[$#] all=[$*]"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	koiOut, _ := runArgv(t, koi, []string{path, "a", "b"})
	bashOut, _ := runArgv(t, bash, []string{path, "a", "b"})
	if koiOut != bashOut {
		t.Errorf("script parameters differ\n  bash: %q\n  koi: %q", bashOut, koiOut)
	}
	if koiOut == "" {
		t.Error("no output: the case cannot detect missing parameters")
	}
}

// The -c path stays POSIX-clean (#41) with parameters in play: this is
// the shape tools use when they spawn $SHELL -c with values they would
// otherwise have to quote into the script text.
func TestDashCWithParametersStaysQuiet(t *testing.T) {
	if testing.Short() {
		t.Skip("differential skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	// A value carrying spaces and a quote is exactly what a caller passes
	// as a parameter instead of interpolating.
	r := compat.Run(context.Background(), bash, koi, compat.Case{
		Name:   "quoting-free parameter passing",
		Script: `set -- "it's a value" second; echo "[$1][$2][$#]"`,
	})
	if !r.Pass {
		t.Errorf("%s\n  bash: %q\n  koi: %q", r.Reason, r.BashOut, r.KoiOut)
	}
}

// runShellArgs runs `sh -c script args...`; runArgv runs `sh argv...`.
// Both return combined output and exit status, matching what
// compat.Run compares, so a hand-rolled case and a harness case measure
// the same thing.
func runShellArgs(t *testing.T, bin, script string, args []string) (string, int) {
	t.Helper()
	return runArgv(t, bin, append([]string{"-c", script}, args...))
}

func runArgv(t *testing.T, bin string, argv []string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, argv...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", bin, err)
	}
	return string(out), code
}
