//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
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
