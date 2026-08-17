//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/blairham/gish/internal/compat"
)

// The builtin matrix (#55): every builtin gish claims to implement, run
// against real bash.
//
// The claim being tested used to live in a list. internal/builtins has
// interpImplemented and interpUnsupported, and its guard exercised four
// of the ~forty names — which is how `alias` sat in a list called
// "implemented" while being a silent no-op. A name in a slice is not
// evidence; running it is.
//
// Differential, reusing the #101 harness so bash on the running machine
// is the oracle. Nothing here encodes what bash "should" do, which
// matters most for the builtins whose behavior is surprising (`shopt`
// defaults, `type` wording, `hash` output). Where gish deliberately
// differs, the case says so and asserts gish's own behavior instead of
// pretending bash agrees.
//
// Scripts are deliberately boring: no PATH lookups beyond the coreutils
// every runner has, no timing, no cwd assumptions. A case that fails
// because the runner is unusual teaches nothing.

// builtinCase is one builtin's exercise.
type builtinCase struct {
	// name is the builtin under test.
	name string
	// script must produce observable output — a builtin that runs and
	// prints nothing cannot be distinguished from one that silently does
	// nothing, which is the exact failure mode being hunted.
	script string
	// gishOnly marks a case where bash is not the oracle, with want
	// holding gish's expected combined output. Every one of these needs
	// a reason: a case that opts out of the oracle to hide a difference
	// is worse than no case.
	gishOnly bool
	want     string
	why      string
	// knownGap records a difference from bash that is real, understood,
	// and not yet fixed. The case still runs and is still compared — it
	// asserts the difference is *still there*, so the day the substrate
	// fixes it the test says so and the entry gets deleted. Deleting the
	// case instead would be how a compat suite quietly stops covering
	// the things it is worst at.
	knownGap string
}

// interpBuiltinCases covers internal/builtins' interpImplemented list.
var interpBuiltinCases = []builtinCase{
	{name: ":", script: `: ; echo "status=$?"`},
	{name: "[", script: `[ 1 = 1 ] && echo yes; [ 1 = 2 ] || echo no`},
	{name: "break", script: `for i in 1 2 3; do [ "$i" = 2 ] && break; echo "$i"; done`},
	{name: "builtin", script: `builtin echo via-builtin`},
	{name: "cd", script: `cd /tmp && pwd | sed 's|/private||'`},
	{name: "command", script: `command echo via-command`},
	{name: "continue", script: `for i in 1 2 3; do [ "$i" = 2 ] && continue; echo "$i"; done`},
	{
		name: "dirs", script: `dirs`,
		knownGap: "bash abbreviates $HOME to ~ in the stack; the interpreter prints absolute paths",
	},
	{name: "echo", script: `echo plain; echo -n no-newline; echo`},
	{name: "eval", script: `eval 'echo evaluated'`},
	{name: "exec", script: `(exec echo execed); echo after`},
	{name: "exit", script: `(exit 3); echo "status=$?"`},
	{name: "export", script: `export FOO=bar; sh -c 'echo "$FOO"'`},
	{name: "false", script: `false; echo "status=$?"`},
	{name: "getopts", script: `set -- -a -b val; while getopts "ab:" o; do echo "opt=$o arg=$OPTARG"; done`},
	{name: "hash", script: `hash sed; echo "status=$?"`},
	{name: "local", script: `f() { local v=inner; echo "$v"; }; v=outer; f; echo "$v"`},
	{name: "mapfile", script: `printf 'a\nb\n' | { mapfile -t arr; echo "${arr[0]}-${arr[1]}"; }`},
	{name: "popd", script: `pushd /tmp >/dev/null; popd >/dev/null; echo "status=$?"`},
	{name: "printf", script: `printf '%s-%d-%05.2f\n' str 42 3.14159`},
	{name: "pushd", script: `pushd /tmp >/dev/null && pwd | sed 's|/private||'`},
	{name: "pwd", script: `cd /tmp && pwd | sed 's|/private||'`},
	{name: "read", script: `printf 'one two\n' | { read a b; echo "$a|$b"; }`},
	{name: "readarray", script: `printf 'x\ny\n' | { readarray -t arr; echo "${arr[1]}"; }`},
	{
		// The one gap here with teeth: the value is protected either way,
		// but gish reports success. A script using readonly as a guard
		// under `set -e` keeps going where bash would stop.
		name: "readonly", script: `readonly R=fixed; echo "$R"; R=changed 2>/dev/null; echo "$R"`,
		knownGap: "assigning to a readonly variable is ignored silently instead of failing with status 1",
	},
	{name: "return", script: `f() { return 7; }; f; echo "status=$?"`},
	{name: "set", script: `set -- a b c; echo "$#-$1-$3"`},
	{name: "shift", script: `set -- a b c; shift; echo "$1-$#"`},
	{
		name: "shopt", script: `shopt -s nullglob; shopt nullglob`,
		knownGap: "bash pads the option name to a fixed column; the interpreter uses a single tab",
	},
	{name: "source", script: `echo 'echo sourced' > "$TMPD/s.sh"; . "$TMPD/s.sh"`},
	{name: "test", script: `test -d /tmp && echo isdir; test -z "" && echo empty`},
	{name: "trap", script: `trap 'echo trapped' EXIT; echo body`},
	{name: "true", script: `true; echo "status=$?"`},
	{name: "type", script: `type cd >/dev/null && echo type-ok`},
	{name: "unset", script: `V=set; unset V; echo "[${V:-gone}]"`},
	{name: "wait", script: `sleep 0.01 & wait; echo waited`},

	// alias and unalias cannot be differential: gish expands aliases in
	// interactive sessions only (#53/#163), and bash -c will not expand
	// even with expand_aliases set, because the whole -c string is parsed
	// before the shopt takes effect. The interactive path is covered by
	// TestAliasFromRCWorksInteractively under a real pty; here the script
	// path's job is to stay quiet and bash-shaped.
	{
		name: "alias", script: `alias a='echo aliased'; a 2>/dev/null; echo "status=$?"`,
		gishOnly: true, want: "status=127\n",
		why: "aliases are interactive-only by design; the script path must not expand",
	},
	{
		name: "unalias", script: `alias a='echo x'; unalias a; echo "status=$?"`,
		gishOnly: true, want: "status=0\n",
		why: "unalias of a defined alias succeeds even where expansion is off",
	},
}

// TestInterpBuiltinsActuallyRun is the regression gate: every builtin in
// the implemented list must do its job.
func TestInterpBuiltinsActuallyRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builtin matrix skipped in -short")
	}
	gish := buildGish(t)
	bash := requireBash(t)

	for _, tc := range interpBuiltinCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			script := "TMPD=" + tmp + "\n" + tc.script
			if tc.gishOnly {
				out, _ := runShell(t, gish, script)
				if out != tc.want {
					t.Errorf("gish %s = %q, want %q (%s)", tc.name, out, tc.want, tc.why)
				}
				return
			}
			r := compat.Run(context.Background(), bash, gish, compat.Case{
				Name: tc.name, Script: script,
			})
			// The oracle has a version. macOS still ships bash 3.2 as
			// /bin/bash, which predates mapfile and readarray by a whole
			// major release, so on that runner bash answers "command not
			// found" for a builtin gish implements correctly. Comparing
			// against that would report gish as broken for being newer.
			//
			// Detected from the oracle's own output rather than by
			// version-gating a list, because the list would be another
			// claim needing its own maintenance — the same mistake this
			// file exists to correct.
			if strings.Contains(r.BashOut, tc.name+": command not found") {
				t.Skipf("bash on this machine has no %s (%s) — no oracle for this case",
					tc.name, bashVersion(t, bash))
			}
			if tc.knownGap != "" {
				if r.Pass {
					t.Errorf("%s now matches bash — the gap closed, so delete this knownGap entry: %s",
						tc.name, tc.knownGap)
				} else {
					t.Logf("known gap: %s\n  bash: %q\n  gish: %q", tc.knownGap, r.BashOut, r.GishOut)
				}
				return
			}
			if !r.Pass {
				t.Errorf("%s differs from bash (%s)\n  bash: %q (exit %d)\n  gish: %q (exit %d)",
					tc.name, r.Reason, r.BashOut, r.BashCode, r.GishOut, r.GishCode)
			}
			// A builtin that prints nothing under both shells proves
			// nothing: agreeing on silence is what a missing builtin
			// looks like.
			if r.BashOut == "" {
				t.Errorf("%s: case produces no output, so it cannot detect a no-op", tc.name)
			}
		})
	}
}

// requireBash skips rather than fails when bash is absent: the oracle is
// the machine's bash, and a machine without one cannot run a
// differential test. Skipping is honest; asserting from memory is not.
// bashVersion reports the oracle's version, for skip messages that say
// which bash could not answer.
func bashVersion(t *testing.T, bash string) string {
	t.Helper()
	out, err := exec.Command(bash, "-c", "echo $BASH_VERSION").Output()
	if err != nil {
		return "unknown version"
	}
	return "bash " + strings.TrimSpace(string(out))
}

func requireBash(t *testing.T) string {
	t.Helper()
	// GISH_TEST_BASH points the oracle at a specific bash. macOS ships
	// 3.2 as /bin/bash while most developers have 5.x first on PATH, so
	// without this the CI-only failures (a builtin the old oracle lacks)
	// cannot be reproduced locally — which is how one shipped.
	if b := os.Getenv("GISH_TEST_BASH"); b != "" {
		return b
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed: nothing to be differential against")
	}
	return bash
}

// runShell runs a script through a shell's -c and returns combined
// output and exit status — the same shape compat.Run compares, so a
// gish-only case and a differential case are measuring the same thing.
func runShell(t *testing.T, bin, script string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, "-c", script)
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
