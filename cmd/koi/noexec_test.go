//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `-n` reads commands without executing them (#233).
//
// The contract callers depend on is the *exit status* and the silence,
// not the wording: a pre-commit hook or CI step runs `sh -n file` and
// branches on the status. So bash is the oracle for the status codes
// here, and the message text is deliberately not compared — koi's parse
// errors keep koi's own shape (#120: bash's interface, not its
// identity), and matching bash's prose would be a lie that then has to
// be maintained against a shell we do not control.
func TestNoExecMatchesBashStatus(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash: the status oracle is unavailable")
	}
	koiBin := buildKoi(t)
	dir := t.TempDir()

	good := filepath.Join(dir, "good.sh")
	if werr := os.WriteFile(good, []byte("echo hi\n"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	bad := filepath.Join(dir, "bad.sh")
	if werr := os.WriteFile(bad, []byte("if\n"), 0o600); werr != nil {
		t.Fatal(werr)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"good script", []string{"-n", good}},
		{"bad script", []string{"-n", bad}},
		{"good -c", []string{"-n", "-c", "echo hi"}},
		{"bad -c", []string{"-n", "-c", "if"}},
		{"clustered -nc", []string{"-nc", "echo hi"}},
		{"clustered bad -nc", []string{"-nc", "if"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, wantCode := runArgv(t, bashBin, tc.args)
			gotOut, gotCode := runArgv(t, koiBin, tc.args)
			if gotCode != wantCode {
				t.Errorf("exit status = %d, bash = %d (koi said %q)", gotCode, wantCode, gotOut)
			}
			// Silence on success is half the contract: a checker that
			// chatters cannot be used in a hook.
			if wantCode == 0 && strings.TrimSpace(gotOut) != "" {
				t.Errorf("printed %q on a clean parse, want nothing", gotOut)
			}
			// And a failure has to say *something*, or the status is the
			// only clue anyone gets.
			if wantCode != 0 && strings.TrimSpace(gotOut) == "" {
				t.Error("reported a syntax error with no message")
			}
		})
	}
}

// The whole point is that nothing runs. A check that executes the script
// it is checking is worse than no check, because CI would run untrusted
// input while believing it was only parsing.
func TestNoExecRunsNothing(t *testing.T) {
	koiBin := buildKoi(t)
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness")

	script := filepath.Join(dir, "touches.sh")
	body := "printf x > " + witness + "\necho done\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if out, code := runArgv(t, koiBin, []string{"-n", script}); code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("check of a valid script: code %d, out %q", code, out)
	}
	if _, err := os.Stat(witness); err == nil {
		t.Fatal("-n executed the script: the witness file exists")
	}

	// The same script through -c, which is the other input path.
	if out, code := runArgv(t, koiBin, []string{"-n", "-c", body}); code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("check of a valid -c string: code %d, out %q", code, out)
	}
	if _, err := os.Stat(witness); err == nil {
		t.Fatal("-n -c executed the command: the witness file exists")
	}
}

// bash ignores -n for interactive shells, and koi copies that rather than
// improving on it: a shell that refused to start because -n was left in a
// terminal profile would be worse than one that runs.
func TestNoExecIsIgnoredWithPipedStdinButStillChecks(t *testing.T) {
	koiBin := buildKoi(t)

	// Piped stdin is not interactive, so it *is* checked — and not run.
	cmd := exec.Command(koiBin, "-n")
	cmd.Stdin = strings.NewReader("echo should-not-run\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("checking piped stdin failed: %v (%s)", err, out)
	}
	if strings.Contains(string(out), "should-not-run") {
		t.Errorf("-n ran piped input: %q", out)
	}

	// And a syntax error on stdin is reported.
	cmd = exec.Command(koiBin, "-n")
	cmd.Stdin = strings.NewReader("if\n")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Errorf("bad piped input exited 0; output %q", out)
	}
}
