//go:build unix

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Namerefs (#277). `declare -n` is how a bash function takes an
// out-parameter — the caller passes a *name*, the callee assigns through
// it — which is the only way bash has to return anything but a string.
//
// nameref.tests was the single biggest item in #277's table: 567 of 589
// lines missed, with a trivial nameref working, which is the shape of one
// construct failing early and taking the file with it. It was five
// separate gaps, and the first one is why the rest were never reached.
//
// bash is the oracle throughout. stdout and exit status are compared;
// stderr is not, since bash prefixes `bash: line N:` and koi keeps its
// own shape (#120).
func TestNamerefsMatchBash(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	// Namerefs landed in bash 4.3. macOS ships 3.2 as /bin/bash, where
	// `declare -n` is an invalid option — so the oracle there answers
	// usage errors for every case and would report koi as broken for
	// implementing the feature. Asked of the oracle rather than derived
	// from a version string, for the reason builtins_matrix_test.go
	// gives: a version-gated list is another claim needing maintenance,
	// and it is wrong the moment a distro backports something.
	if probe, _ := runArgv(t, bashBin, []string{"-c", "declare -n r=x"}); strings.Contains(probe, "invalid option") {
		t.Skipf("bash on this machine has no namerefs (%s) — no oracle for these cases", bashVersion(t, bashBin))
	}

	cases := []struct{ name, script string }{
		{
			// The one that cascaded. `declare -n foo` with no value
			// *promotes* an existing variable: its current value is the
			// name it now points at. koi kept the value and dropped the
			// attribute, so every later read gave the target's name
			// where bash gives its value — and nameref.tests diverges on
			// its very first line of output.
			"declare -n with no value promotes an existing variable",
			`bar=one; foo=bar; typeset -n foo; echo ${foo}`,
		},
		{
			"assigning through a promoted nameref reaches the target",
			`bar=one; foo=bar; declare -n foo; foo=two; echo "$bar"`,
		},
		{
			"declare -n with a value still works",
			`bar=one; declare -n foo=bar; echo $foo; foo=two; echo $bar`,
		},
		{
			// Writing it again retargets. Following the existing
			// reference instead built a chain r->a->b, which resolves to
			// the right value and is the wrong shape — invisible until
			// something prints the attributes.
			"declare -n retargets an existing nameref",
			`a=1; b=2; declare -n r=a; declare -n r=b; declare -p r; echo $r`,
		},
		{
			"a nameref to a nameref keeps its own target",
			`a=1; declare -n r=a; declare -n s=r; echo $s; declare -p s`,
		},
		{
			// A nameref with no target at all. This one crashed koi
			// outright once the attribute started being set: resolving it
			// asked the environment for the variable named "".
			"declare -n on an unset name has no target",
			`typeset -n foo; declare -p foo; echo "[${foo}]"`,
		},
		{
			"assigning to a target-less nameref keeps the attribute",
			`typeset -n foo; foo=x; declare -p foo; echo "bar=$bar"`,
		},
		{
			// Declared but never set prints bare, and the rule is Set
			// rather than empty: `foo=` is set and prints `foo=""`.
			"declare -p prints a declared-but-unset name bare",
			`declare -x e; declare -i n; foo=; declare -p e n foo`,
		},
		{
			"a bare declare -n lists the namerefs",
			`b=1; declare -n f=b; declare -n g=b; declare -n`,
		},
		{"a bare declare -n with none lists nothing", `declare -n; echo "rc=$?"`},
		{
			// +n detaches. The order is the subtlety: the assignment goes
			// through the reference first, and only then is the attribute
			// removed, so the variable keeps the target's *name* as its
			// own value.
			"+n assigns through the reference, then detaches",
			`bar=one; foo=bar; typeset -n foo; typeset +n foo=other; echo "foo=$foo bar=$bar"; declare -p foo`,
		},
		{
			"+n with no value just detaches",
			`bar=one; foo=bar; typeset -n foo; typeset +n foo; echo "foo=$foo bar=$bar"`,
		},
		{"+n on a plain variable is a no-op", `x=1; declare +n x; declare -p x`},
		{
			// unset follows the reference: it removes what the nameref
			// points at and leaves the reference standing. koi had this
			// exactly backwards, so a later use of the name was an
			// ordinary variable and the rest of the script drifted.
			"unset removes the target and keeps the reference",
			`bar=one; typeset -n foo=bar; unset foo; declare -p foo; echo "[$foo][$bar]"`,
		},
		{
			"unset -n removes the reference and keeps the target",
			`bar=one; typeset -n foo=bar; unset -n foo; declare -p bar; echo "[$bar]"`,
		},
		{
			// The sequence from nameref.tests that this all has to add up
			// to: after unsetting both, the name is free again.
			"the reference survives its target being unset",
			`bar=one; typeset -n foo=bar; foo=two; unset foo bar; foo=bar; bar=one; echo "expect <$foo>"`,
		},
		{
			// What namerefs are actually for: an out-parameter.
			"a function assigns through a nameref out-parameter",
			`setval(){ typeset -n ref=$1; ref="$2"; }; setval out hello; echo "$out"`,
		},
		{
			"a function reads through a nameref parameter",
			`echoval(){ typeset -n ref=$1; printf "%s\n" $ref; }; foo=bar; bar=one; echoval foo`,
		},
		{"unset on a plain variable is unchanged", `x=1; unset x; echo "[$x]"`},
		{"unset of an array element is unchanged", `a=(1 2 3); unset a[1]; declare -p a`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compareNamerefCase(t, bashBin, koiBin, tc.script)
		})
	}
}

func compareNamerefCase(t *testing.T, bashBin, koiBin, script string) {
	t.Helper()
	wantOut, wantCode := runStdout(t, bashBin, script)
	gotOut, gotCode := runStdout(t, koiBin, script)
	if gotOut != wantOut {
		t.Errorf("output = %q, bash = %q", gotOut, wantOut)
	}
	if gotCode != wantCode {
		t.Errorf("exit status = %d, bash = %d", gotCode, wantCode)
	}
}

// runStdout keeps stdout apart from stderr, which runArgv does not.
//
// Appending `2>/dev/null` to the script was the first attempt and is
// wrong: a redirect binds to the last command of a list, not to the list,
// so every earlier diagnostic still landed on the comparison. Capturing
// the streams separately is the only version that means what it says.
func runStdout(t *testing.T, bin, script string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, "-c", script)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, io.Discard
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", bin, err)
	}
	return stdout.String(), code
}

// What a reference's diagnostics say and what they cost (#610).
//
// Differential, and it has to be a script *file*: the two halves this
// pins are both invisible in a `-c` string. A command string has no file
// to name (#120, #571), so the `source: line N: ` prefix the two
// indirect-expansion messages now carry would be empty; and a command
// string is a single input unit, so "abandon this line and keep reading"
// (#469) and "abandon everything" look identical there.
//
// The `declare` lines are the other half of that split, measured by
// #308 and #582: a refused declaration answers 1 and carries on to the
// next command, where the plain assignment above it loses the rest of
// its line.
const namerefDiagScript = `bar=one
declare -n ref=bar
readonly ref
declare -p ref bar
ref=two; echo "unreachable after a refused assignment"
echo "the next line runs"
declare ref=three; echo "declare answered $?"
declare -a arr=(x y); declare -n arr=bar; echo "declare -n over an array answered $?"
declare -p arr
declare -n elem[3]=bar; echo "a subscripted reference answered $?"
unset nosuch_target
declare -n ptr=nosuch_target
. ./nrlib.sh
echo "end"
`

// Sourced, because a sourced file is the second place the location has
// to be right and the first place the abandonment used to be too wide
// (#585): the library must lose the rest of each offending line and keep
// reading the ones after it.
const namerefDiagLib = `echo "${!nosuch_var}"; echo "unreachable after a bad indirect"
echo "the library keeps reading"
bad='a b'
echo "${!bad}"; echo "unreachable after a bad name"
echo "library end"
`

func TestNameRefDiagnosticsMatchBash(t *testing.T) {
	if testing.Short() {
		t.Skip("differential nameref diagnostics skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	tmp := t.TempDir()
	for name, body := range map[string]string{
		"nrmain.sh": namerefDiagScript,
		"nrlib.sh":  namerefDiagLib,
	} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Run from inside tmp so both shells are handed the same relative
	// path: bash names a script as it was written, not as it resolves.
	bashOut, bashCode := runInDir(t, tmp, bash, "./nrmain.sh")
	koiOut, koiCode := runInDir(t, tmp, koi, "./nrmain.sh")
	if bashOut != koiOut || bashCode != koiCode {
		t.Errorf("nameref diagnostics differ from bash\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
			bashOut, bashCode, koiOut, koiCode)
	}
	// Two shells agreeing on an answer that never reached the cases
	// would pass while proving nothing. The oracle has to show the
	// located diagnostic, the refusal that costs a line, the refusal
	// that does not, and the line *after* each — which is the half a
	// too-wide abandonment would swallow.
	for _, want := range []string{
		"./nrlib.sh: line 1: nosuch_var: invalid indirect expansion",
		"./nrlib.sh: line 4: a b: invalid variable name",
		"./nrmain.sh: line 5: bar: readonly variable",
		"declare: arr: reference variable cannot be an array",
		"declare: elem[3]: reference variable cannot be an array",
		"the next line runs",
		"the library keeps reading",
		"library end",
	} {
		if !strings.Contains(bashOut, want) {
			t.Errorf("the oracle's output lacks %q, so this case cannot detect a regression: %q",
				want, bashOut)
		}
	}
	// The mirror of the above: an assignment refused through a reference
	// must not happen, and the line it was on must not survive it.
	// Asserting only the message would pass against the original bug,
	// where koi printed nothing and assigned anyway.
	for _, unwanted := range []string{
		"unreachable after a refused assignment",
		"unreachable after a bad indirect",
		"unreachable after a bad name",
	} {
		if strings.Contains(bashOut, unwanted) {
			t.Errorf("the oracle printed %q, so this case is asserting the wrong rule: %q",
				unwanted, bashOut)
		}
	}
}
