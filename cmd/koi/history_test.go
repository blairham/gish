//go:build unix

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The `history` builtin (#277).
//
// koi already had the history engine — a store with live cross-session
// reload (#40), a completion engine, and the expansion `!!` needs — and
// none of it was reachable from a script, which got `history: unsupported
// builtin` and exit 2 where bash answers silently and exit 0. The engines
// existed; the surface did not.
//
// bash is the oracle for **stdout and exit status**. stderr is not
// compared: bash prefixes its diagnostics with `bash: line N:` and koi
// keeps its own shape (#120), and on file errors bash says nothing at all
// where koi reports what went wrong.
func TestHistoryBuiltinMatchesBash(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	// minBash guards the two rows whose *bash* behavior changed. macOS
	// ships 3.2 as /bin/bash, and there the oracle asserts the opposite
	// of what koi means to do — the situation printf_test.go already
	// handles this way, and the reason KOI_TEST_BASH exists.
	//
	// The bound is 5 rather than bisected: only 3.2 and 5.3 were
	// available to measure, so 4.x is untested and a conservative bound
	// costs coverage there and never correctness.
	cases := []struct {
		name, script string
		minBash      int
	}{
		// The starting point: bash succeeds silently on an empty list.
		{"an empty list prints nothing and succeeds", `history; echo "rc=$?"`, 0},

		{"-s appends and the list is numbered from 1", `history -s "echo one"; history -s "echo two"; history`, 0},
		{"a count shows the newest n, keeping their numbers", `history -s "echo one"; history -s "echo two"; history 1`, 0},
		{"a count larger than the list shows all of it", `history -s a; history 9`, 0},
		{"-c clears it", `history -s a; history -s b; history -c; history; echo "rc=$?"`, 0},
		{"-d removes one and renumbers", `history -s a; history -s b; history -d 1; history`, 0},
		{"-d counts back from the newest when negative", `history -s a; history -s b; history -d -1; history`, 5},
		{"-s with no argument is a no-op", `history -s; history; echo "rc=$?"`, 0},

		// -p is the half that connects the builtin to the expander koi
		// already had (#96), so `history -p '!!'` and typing `!!` cannot
		// disagree about what `!!` means.
		{"-p expands without running or recording", `history -s "echo hi"; history -p '!!'; history`, 0},
		{"-p on an empty list fails", `history -p '!!'; echo "rc=$?"`, 0},
		{"-p with no argument succeeds silently", `history -p; echo "rc=$?"`, 0},

		// Errors. bash prints its usage line for a bad *option* and not
		// for a bad operand, which was measured rather than assumed.
		{"an invalid option exits 2", `history -x; echo "rc=$?"`, 0},
		{"a non-numeric count exits 2", `history abc; echo "rc=$?"`, 5},
		{"-d wants a number", `history -d abc; echo "rc=$?"`, 0},
		{"-d out of range exits 1", `history -d 99; echo "rc=$?"`, 0},
		{"-d needs an argument", `history -d; echo "rc=$?"`, 0},
		{"only one of -anrw at a time", `history -r -w /dev/null; echo "rc=$?"`, 0},
	}

	major := bashMajor(t, bashBin)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if major < tc.minBash {
				t.Skipf("oracle is bash %d: this behavior changed after it", major)
			}
			compareStdout(t, bashBin, koiBin, tc.script)
		})
	}
}

// -r and -w are stateless — read a plain file into the list, write the
// list out one command per line — so koi implements them completely.
// They run under `cd` because the paths have to resolve against the
// *shell's* directory, not the Go process's, and that is a bug which only
// shows up once a script has changed directory.
func TestHistoryFileFormsMatchBash(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	cases := []struct{ name, script string }{
		{"-w writes the list one command per line", `history -s "echo one"; history -s "echo two"; history -w out; cat out`},
		{"-r reads a file into the list", `printf 'a\nb\n' > in; history -r in; history`},
		{"-r then -w round-trips", `printf 'a\nb\n' > in; history -r in; history -w out2; cat out2`},
		{"-r of an empty file adds nothing", `: > empty; history -r empty; history; echo "rc=$?"`},
		{"-r of a missing file fails", `history -r nosuchfile; echo "rc=$?"`},
		{"-w into a missing directory fails", `history -w nosuchdir/x; echo "rc=$?"`},
		{"no operand uses $HISTFILE", `history -s x; history -w; cat "$HISTFILE"`},
		// The one that catches resolving against the wrong directory.
		{"a relative path follows the shell's cwd", `mkdir -p d; cd d; history -s q; history -w rel; cat rel`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Both shells get the same scratch dir and HISTFILE, and
			// neither gets the developer's home — same rule as #260.
			script := "cd " + dir + "\n" + tc.script
			env := []string{"HOME=" + dir, "HISTFILE=" + filepath.Join(dir, "hf")}
			compareStdoutEnv(t, bashBin, koiBin, script, env)
		})
	}
}

// `-a` and `-n` are the incremental pair — "the lines new since last
// time" — which needs a per-file read position over a per-process list.
// koi's history is a store shared live across sessions (#40), so they
// report what they do not do rather than succeeding without doing it.
// This is a deliberate divergence from bash, which is why it is asserted
// against koi alone rather than differentially.
func TestHistoryIncrementalFormsRefuseWithAReason(t *testing.T) {
	koiBin := buildKoi(t)
	dir := t.TempDir()

	for _, flag := range []string{"-a", "-n"} {
		t.Run(flag, func(t *testing.T) {
			out, code := runArgv(t, koiBin, []string{"-c", "history " + flag + " " + filepath.Join(dir, "f")})
			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			// A refusal has to say why, or it is indistinguishable from a
			// bug — the whole complaint #277 makes about this surface.
			if !strings.Contains(out, "not implemented") || !strings.Contains(out, "#40") {
				t.Errorf("refusal does not explain itself: %q", out)
			}
		})
	}
}

// compareStdout runs a script through both shells and requires the same
// stdout and exit status. See the note on stderr above.
func compareStdout(t *testing.T, bashBin, koiBin, script string) {
	t.Helper()
	compareStdoutEnv(t, bashBin, koiBin, script, nil)
}

func compareStdoutEnv(t *testing.T, bashBin, koiBin, script string, env []string) {
	t.Helper()
	wantOut, wantCode := runScriptEnv(t, bashBin, script, env)
	gotOut, gotCode := runScriptEnv(t, koiBin, script, env)
	if gotOut != wantOut {
		t.Errorf("stdout = %q, bash = %q", gotOut, wantOut)
	}
	if gotCode != wantCode {
		t.Errorf("exit status = %d, bash = %d", gotCode, wantCode)
	}
}

func runScriptEnv(t *testing.T, bin, script string, env []string) (string, int) {
	t.Helper()
	// A scratch HOME by default so neither shell reads the developer's
	// real history — bash loads $HISTFILE readily, and a test that
	// scored against one person's home would not be reproducible.
	base := []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "HOME=" + t.TempDir()}
	return runArgvEnv(t, bin, []string{"-c", script}, append(base, env...))
}

// runArgvEnv runs with the environment given rather than inherited, and
// keeps stdout apart from stderr.
//
// Both differ from runArgv on purpose. runArgv appends to os.Environ(),
// which is right for most cases and wrong for this one: bash reads
// $HISTFILE readily, so a history test that inherits the developer's
// environment is scored against their real shell history and is not
// reproducible between machines — the mistake #260 fixed in the compat
// runner, and one this suite can make just as easily.
//
// And runArgv combines the streams, which would make every diagnostic
// part of the comparison. bash prefixes its own with `bash: line N:` and
// koi keeps its own shape (#120), so only stdout can be compared.
func runArgvEnv(t *testing.T, bin string, argv, env []string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, argv...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
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

// Interactively, `history` has to report what the user can actually see
// with the up-arrow — the store — rather than an empty list.
//
// That is the half a `koi -c` test cannot reach: in a script the store is
// absent and the list starts empty, so every case above would pass just
// as well against a builtin that never read the store at all.
func TestHistoryListsTheInteractiveSession(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.runLine("echo alpha-probe")

	// The buffer is cleared before the send, so finding the marker after
	// it means the *listing* carried it — the echo of "history" cannot.
	out := s.runProbe("history", "alpha-probe")

	// Matched as "digits, whitespace, the command" rather than by
	// splitting a line into fields: the harness leaves OSC sequences in
	// its stripped output, so the listing arrives as
	// "\x1b]133;C\x1b\\    1  echo alpha-probe" and the first field is
	// not the number. What is being asserted is the numbering, and this
	// asserts exactly that.
	if !numberedHistoryLine.MatchString(out) {
		t.Fatalf("history did not list the command just run, numbered:\n%s", out)
	}
}

var numberedHistoryLine = regexp.MustCompile(`\d+\s+echo alpha-probe`)
