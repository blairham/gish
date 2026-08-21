//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// How fatal is each error class, differentially (#277).
//
// bash's answer is not one rule: an invalid indirect expansion and a
// readonly assignment abandon the current input unit and keep reading —
// a script file continues at the next command, while -c loses its
// remainder — but an unbound variable under `set -u` ends the shell in
// both shapes. koi answered `exiting` for the indirect, which cost a
// script every line after one bad probe (nameref3.sub forfeited its
// second half to line 29).
//
// The messages deliberately differ (#120), so what is compared is the
// behavior a script acts on: whether the next command ran, and the exit
// status. bash is the oracle for both.

func runFatalityScript(t *testing.T, shell, body string) (sawAfter bool, exit int) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "case.sh")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shell, path)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb"}
	out, err := cmd.CombinedOutput()
	exit = 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	}
	return strings.Contains(string(out), "AFTER-MARK"), exit
}

func TestExpansionErrorFatalityMatchesBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), requireBash(t)

	tests := []struct {
		name, body string
	}{
		{"invalid indirect continues in a file", "unset foo\necho \"${!foo-def}\"\necho AFTER-MARK\n"},
		{"invalid name target continues in a file", "foo='a b'\necho \"${!foo}\"\necho AFTER-MARK\n"},
		{"unset nameref indirect continues in a file", "bar=one\ntypeset -n foo=bar\nunset -n foo\necho \"${!foo-def}\"\necho AFTER-MARK\n"},
		{"readonly assignment continues in a file", "readonly r=1\nr=2\necho AFTER-MARK\n"},
		// `${x:}` is a slice with neither half: bash reads it and
		// reports "bad substitution" while expanding, so the command is
		// lost and the file carries on (#277). koi refused it while
		// parsing, which lost every line after it instead.
		{"bad substitution continues in a file", "x=abc\necho \"[${x:}]\"\necho AFTER-MARK\n"},
		{"nounset ends the file", "set -u\necho \"$nope\"\necho AFTER-MARK\n"},
		// #602's two halves, which is why this table is where they are
		// pinned: the wording is identical and the fatality is not. A
		// suffix no operator spells is the recoverable kind — the
		// command is lost, the file carries on — while a `${x@…}`
		// transform bash has no letter for ends the shell exactly as
		// nounset does. In a -c string both look the same, so only a
		// script file can tell them apart.
		{"bad operator suffix continues in a file", "H=1\necho ${H*}\necho AFTER-MARK\n"},
		{"bad parameter name continues in a file", "set -- a\necho ${#1xyz}\necho AFTER-MARK\n"},
		{"bad positional suffix continues in a file", "set -- a b c\necho \"${@*}\"\necho AFTER-MARK\n"},
		{"empty @ transform ends the file", "V=1\necho ${V@}\necho AFTER-MARK\n"},
		{"unknown @ transform ends the file", "x=hello\necho ${x@nope}\necho AFTER-MARK\n"},
		{"@ transform on set positionals ends the file", "set -- a b c\necho \"${*@}\"\necho AFTER-MARK\n"},
		// The same transform on a parameter with no value is not an
		// error at all, so the file runs to the end at status 0.
		{"unknown @ transform on an unset name is no error", "unset x\necho \"[${x@nope}]\"\necho AFTER-MARK\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bAfter, bExit := runFatalityScript(t, bash, tc.body)
			kAfter, kExit := runFatalityScript(t, koi, tc.body)
			if bAfter != kAfter {
				t.Errorf("next-command-ran: koi=%v bash=%v", kAfter, bAfter)
			}
			if bExit != kExit {
				t.Errorf("exit: koi=%d bash=%d", kExit, bExit)
			}
		})
	}
}
