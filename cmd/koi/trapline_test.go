//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Where the DEBUG and RETURN traps say they are (#614).
//
// `print_return_trap $LINENO` is bash's own debugger idiom — its
// dbg-support.tests is built on it — and koi answered the line the
// *trap* was written on, so every frame in a run reported the same
// number: plausible, constant, and useless for the one thing the trap is
// read for.
//
// These cases live here rather than in interp's table because the whole
// point of them is a `source`, and a sourced file needs a file to exist
// — which is also the frame with the interesting rule, since its RETURN
// action runs with the frame already *popped*, unlike a function's. The
// three variables are read alongside the line because the line alone
// cannot tell a popped frame from an unpopped one.
func TestTrapLinesMatchBash(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	// A library that defines a function, runs something at its own top
	// level, and calls the function — so one `source` covers a source
	// frame, a function frame inside it, and a function whose *definition*
	// file is not the file that calls it.
	const lib = "libfn() {\n  echo libfn-body\n}\necho lib-top\nlibfn\n"

	// The trap actions read three frame variables beside $LINENO:
	// FUNCNAME[1] names the frame the action is running in, BASH_SOURCE[0]
	// the file it is running in, and BASH_COMMAND the last command the
	// frame ran. A line number on its own would pass whether or not the
	// source frame had been popped first.
	const retTrap = `trap 'echo "R:$LINENO fn=${FUNCNAME[1]:-none} src=${BASH_SOURCE[0]##*/} cmd=[$BASH_COMMAND]"' RETURN`
	const dbgTrap = `trap 'echo "D:$LINENO fn=${FUNCNAME[1]:-none} src=${BASH_SOURCE[0]##*/}"' DEBUG`

	cases := []struct{ name, script string }{
		{
			// A source's RETURN reports the line the `source` was
			// written on, in the *caller's* file, with the frame gone —
			// so `fn` is the caller's and BASH_COMMAND is the `source`
			// itself rather than whatever the file ran last.
			"a source at the top level",
			"set -T\n" + retTrap + "\necho pre\n. ./lib.sh\necho post\n",
		},
		{
			// The same source one frame down: the action lands back in
			// `outer` on outer's own line, and then outer's own return
			// reports the line its body starts on.
			"a source inside a function",
			"set -T\n" + retTrap + "\nouter() {\n  . ./lib.sh\n}\nouter\n",
		},
		{
			// A function defined in the library and called from the main
			// script reports the *library's* body line, which is the
			// same rule BASH_SOURCE follows for a function (#266).
			"a function defined in a sourced library",
			"set -T\n. ./lib.sh\n" + retTrap + "\necho mid\nlibfn\n",
		},
		{
			// The DEBUG side of the same shape: a sourced file's
			// commands trace under functrace, its function gets an entry
			// event on the line its body starts on, and every line is
			// attributed to the file it lives in.
			"tracing into a sourced file",
			"set -T\n" + dbgTrap + "\n. ./lib.sh\n",
		},
	}

	// Deliberately absent: both traps installed at once, which is how a
	// debugger installs them. bash traces the RETURN trap's own action
	// string before running it and traces the function that action calls,
	// and koi suppresses DEBUG inside every trap rather than inside the
	// DEBUG trap alone — #630, measured there rather than asserted as a
	// failure here.

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "lib.sh"), []byte(lib), 0o600); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(dir, "script.sh")
			if err := os.WriteFile(script, []byte(tc.script), 0o600); err != nil {
				t.Fatal(err)
			}
			wantOut, wantCode := runInDir(t, dir, bashBin, "./script.sh")
			gotOut, gotCode := runInDir(t, dir, koiBin, "./script.sh")
			if gotOut != wantOut {
				t.Errorf("output =\n%s\nbash =\n%s", gotOut, wantOut)
			}
			if gotCode != wantCode {
				t.Errorf("exit status = %d, bash = %d", gotCode, wantCode)
			}
			// A trace test that only compares strings passes vacuously
			// against a shell that traced nothing *and* a bash that
			// traced nothing, so pin that both actually fired.
			if wantOut == "" {
				t.Fatal("bash printed nothing, so this case asserts nothing")
			}
		})
	}
}
