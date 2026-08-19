//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `trap ... RETURN` (#295), the cleanup idiom:
//
//	with_lock() {
//	  trap 'rm -f "$lockfile"' RETURN
//	  ...
//	}
//
// which is how a function releases a resource on every exit path without
// repeating itself before each `return`. koi refused RETURN outright, so
// the lock file survived the function.
//
// Its semantics are almost entirely about *which frames* the trap is
// reachable from, and that is not guessable — every row here was measured
// against real bash first, and bash is the oracle for all of them. bash
// 3.2 already has RETURN (it landed in 3.0), so even macOS's system bash
// is a valid oracle for this.
func TestTrapReturnMatchesBash(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	// A file to source. `source` is half of what RETURN fires for, and
	// the half with the different inheritance rule.
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib.sh")
	if err := os.WriteFile(lib, []byte("echo sourced\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, script string }{
		{
			"fires when the function returns",
			`f(){ trap "echo left $FUNCNAME" RETURN; echo in; }; f`,
		},
		{
			// The trap must not eat the return value; that would break
			// every caller of a function that has one.
			"the return status survives the trap",
			`f(){ trap "echo left" RETURN; return 5; }; f; echo "rc=$?"`,
		},
		{
			// Single-quoted so $? is read when the trap runs, not when it
			// is set — which is the only version that is useful.
			"$? inside the trap is the last command's",
			`f(){ trap 'echo status=$?' RETURN; false; }; f; echo "after=$?"`,
		},
		{"it fires on every call, not just the first", `f(){ trap "echo T" RETURN; :; }; f; f`},
		{"trap -p lists it", `f(){ trap "echo T" RETURN; :; }; f; trap -p RETURN`},
		{
			// The inheritance rule, and the one that is easiest to get
			// backwards: a function does *not* inherit RETURN.
			"a top-level trap does not fire for a function",
			`trap "echo R" RETURN; f(){ echo in; }; f; echo done`,
		},
		{"set -T makes a function inherit it", `set -T; trap "echo R" RETURN; f(){ echo in; }; f`},
		{
			// ...but a sourced file inherits it with no -T at all.
			"a top-level source does fire it",
			`trap 'echo R' RETURN; . ` + lib + `; echo after`,
		},
		{
			// And the two rules compose: f does not inherit, so the
			// source inside f has nothing to inherit either.
			"a source inside an uninheriting function fires nothing",
			`trap 'echo R' RETURN; f(){ . ` + lib + `; echo in-f; }; f`,
		},
		{
			"a trap set inside a function fires for that function",
			`f(){ trap 'echo R' RETURN; }; f; g(){ echo g; }; g`,
		},
		{
			// The handler is global and nothing puts it back, so it is
			// still reachable at top level afterwards. This is the row
			// that rules out saving and restoring the handler itself.
			"a trap set inside a function outlives it",
			`f(){ trap 'echo R' RETURN; :; }; f; . ` + lib,
		},
		{
			// And this is the row that rules out *not* restoring the
			// reachability flag: g turning it off must not silence f.
			"a nested call does not silence its caller's trap",
			`f(){ trap 'echo R' RETURN; g; }; g(){ echo g; }; f`,
		},
		{"under -T the nested call fires too", `set -T; f(){ trap 'echo R' RETURN; g; }; g(){ echo g; }; f`},
		{"the trap can unset itself", `f(){ trap 'trap - RETURN; echo once' RETURN; :; }; f; f`},
		{
			"a source inside a function fires for both",
			`f(){ trap 'echo R' RETURN; . ` + lib + `; echo after-source; }; f`,
		},
		{
			// The idiom the issue is written around, end to end.
			"the cleanup idiom releases on every exit path",
			`d=` + dir + `
with_lock(){ trap 'rm -f "$d/lock"; echo released' RETURN; : > "$d/lock"; [ -n "$1" ] && return 1; echo work; }
with_lock; echo "held=$(ls "$d"/lock 2>/dev/null | wc -l | tr -d ' ')"
with_lock early; echo "held=$(ls "$d"/lock 2>/dev/null | wc -l | tr -d ' ')"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantOut, wantCode := runArgv(t, bashBin, []string{"-c", tc.script})
			gotOut, gotCode := runArgv(t, koiBin, []string{"-c", tc.script})
			if gotOut != wantOut {
				t.Errorf("output = %q, bash = %q", gotOut, wantOut)
			}
			if gotCode != wantCode {
				t.Errorf("exit status = %d, bash = %d", gotCode, wantCode)
			}
		})
	}
}

// `trap -p` lists RETURN last, after EXIT, DEBUG and ERR. Checked on its
// own because the differential cases above would need commands that exist
// in both shells to compare the *firing*, and here only the listing
// matters.
func TestTrapListingIncludesReturnLast(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	const script = `trap ":" EXIT; trap ":" DEBUG; trap ":" ERR; trap ":" RETURN; trap -p`
	want := trapLines(runArgvOut(t, bashBin, script))
	got := trapLines(runArgvOut(t, koiBin, script))
	if got != want {
		t.Errorf("trap -p =\n%s\nbash =\n%s", got, want)
	}
	if !strings.HasSuffix(want, "RETURN") {
		t.Fatalf("this bash does not list RETURN last, so the assertion above is not testing what it claims:\n%s", want)
	}
}

func runArgvOut(t *testing.T, bin, script string) string {
	t.Helper()
	out, _ := runArgv(t, bin, []string{"-c", script})
	return out
}

// trapLines keeps only the `trap --` listing, dropping whatever the
// pseudo-signals printed while the script ran.
func trapLines(out string) string {
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "trap -- ") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}
