//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Reading a script the way bash reads one (#450, #516).
//
// interp's own table cannot reach either of these: it parses a whole
// script and then runs it, which is exactly the thing being changed
// here. What a line can change about the rest of the script is only
// observable through the shell, so the cases are whole scripts run
// through both shells with their output compared.
//
// bash is the oracle; nothing below encodes what bash ought to print.
// The two places koi deliberately answers differently — the wording of
// a syntax error (#120) — are asserted against koi instead, and say so.

// runShellScript runs path under a shell and returns its combined output
// with the status appended, so a divergence in either fails.
func runShellScript(t *testing.T, shell, path string, stdin *os.File) string {
	t.Helper()
	cmd := exec.Command(shell, path)
	if stdin != nil {
		cmd = exec.Command(shell)
		cmd.Stdin = stdin
	}
	cmd.Dir = filepath.Dir(path)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb"}
	out, _ := cmd.CombinedOutput() // a non-zero status is part of the comparison
	return string(out) + "status=" + strconv.Itoa(cmd.ProcessState.ExitCode()) + "\n"
}

// runStdinScript hands the script to the shell as standard input, which
// is the one arrangement where redirecting fd 0 switches the command
// stream.
func runStdinScript(t *testing.T, shell, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return runShellScript(t, shell, path, f)
}

// requireModernBash skips when the oracle predates the behavior being
// compared. bash's posix-mode quote rule arrived with the 4.2
// compatibility level, and macOS still ships 3.2 as /bin/bash — so
// without this the CI runner would compare against a shell that answers
// the old way and report koi as wrong.
func requireModernBash(t *testing.T) string {
	t.Helper()
	bash := requireBash(t)
	out, err := exec.Command(bash, "-c", "echo ${BASH_VERSINFO[0]}").Output()
	if err != nil {
		t.Skipf("cannot ask %s its version: %v", bash, err)
	}
	if major, _ := strconv.Atoi(strings.TrimSpace(string(out))); major < 5 {
		t.Skipf("%s is bash %s.x, which predates the posix-mode quote rule", bash, strings.TrimSpace(string(out)))
	}
	return bash
}

// A quote inside a double-quoted ${...} stops being special once a
// script turns on posix mode — which means a line changes how the *rest
// of the script is tokenized*, so the shell cannot have parsed it yet.
func TestPosixModeChangesHowLaterLinesAreRead(t *testing.T) {
	t.Parallel()
	bash := requireModernBash(t)
	koi := buildKoi(t)

	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{
			// quote1.sub's case, which is where this was found.
			"a later line",
			map[string]string{"s.sh": "set -o posix\necho \"foo ${IFS+'bar} baz\"\necho after\n"},
		},
		{
			// The pattern operators keep their quotes special in posix
			// mode, since there the quoting decides what the pattern
			// matches. Both shells expand this, neither errors.
			"pattern operators keep quoting",
			map[string]string{"s.sh": "set -o posix\nx=aXb\necho \"${x#'a'}\" \"${x#a}\"\n"},
		},
		{
			"turned back off",
			map[string]string{"s.sh": "set -o posix\nx=y\necho \"${x+'}\"\nset +o posix\necho done\n"},
		},
		{
			// A sourced file is read by the interpreter rather than by
			// the shell around it, so it needs the same treatment —
			// the #259 lesson about where a fix has to live.
			"inside a sourced file",
			map[string]string{
				"s.sh":   ". ./lib.sh\necho after\n",
				"lib.sh": "set -o posix\nx=y\necho \"${x+'lit}\"\n",
			},
		},
		{
			// posix mode set before the source reaches the sourced file.
			"before a source",
			map[string]string{
				"s.sh":   "set -o posix\nx=y\n. ./lib.sh\n",
				"lib.sh": "echo \"${x+'lit}\"\n",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := write(t, tc.files, "s.sh")
			want := runShellScript(t, bash, path, nil)
			if got := runShellScript(t, koi, path, nil); got != want {
				t.Errorf("koi = %q, bash = %q", got, want)
			}
		})
	}
}

// The mode reaches the next line, not the rest of its own: bash reads a
// whole line before running any of it. Both shells fail here, and the
// message is koi's own (#120), so the status is what is compared.
func TestPosixModeDoesNotChangeItsOwnLine(t *testing.T) {
	t.Parallel()
	bash := requireModernBash(t)
	koi := buildKoi(t)

	path := write(t, map[string]string{
		"s.sh": "x=y\nset -o posix; echo \"${x+'}\"\n",
	}, "s.sh")
	bashOut := runShellScript(t, bash, path, nil)
	koiOut := runShellScript(t, koi, path, nil)
	if !strings.HasSuffix(bashOut, "status=2\n") {
		t.Fatalf("the oracle did not reject the line: %q", bashOut)
	}
	if !strings.HasSuffix(koiOut, "status=2\n") {
		t.Errorf("posix mode reached its own line: %q", koiOut)
	}
}

// When the shell reads its commands from standard input, redirecting fd
// 0 redirects the command stream: the rest of the script comes from the
// new file.
func TestRedirectingStdinSwitchesTheCommandStream(t *testing.T) {
	t.Parallel()
	bash := requireBash(t)
	koi := buildKoi(t)

	const inner = "echo from-inner\necho inner-two\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{
			// redir.tests' redir7, which is why bash's own suite has it.
			"exec 0< file",
			map[string]string{
				"s.sh":     "echo one\nexec 0< ./inner.sh\necho never\n",
				"inner.sh": inner,
			},
		},
		{
			// The rest of the *line* runs first, since bash had already
			// read it. Only then does the stream change.
			"the rest of the line runs first",
			map[string]string{
				"s.sh":     "echo one\nexec 0< ./inner.sh; echo tail\necho never\n",
				"inner.sh": inner,
			},
		},
		{
			// `exec <f` is the same operation spelled without the zero,
			// which is why the switch is detected by asking what fd 0
			// points at rather than by watching for a spelling.
			"exec < file",
			map[string]string{
				"s.sh":     "echo one\nexec < ./inner.sh\necho never\n",
				"inner.sh": inner,
			},
		},
		{
			"a group redirect around the rest",
			map[string]string{
				"s.sh":     "echo one\n{ exec 0< ./inner.sh; }\necho never\n",
				"inner.sh": inner,
			},
		},
		{
			// Any other descriptor is an ordinary redirection.
			"another descriptor does not switch",
			map[string]string{
				"s.sh":     "echo one\nexec 3< ./inner.sh\nread line <&3; echo \"got=$line\"\n",
				"inner.sh": inner,
			},
		},
		{
			// A switch is followed again: the new stream is the command
			// stream in exactly the same way.
			"twice",
			map[string]string{
				"s.sh":      "echo one\nexec 0< ./inner.sh\necho never\n",
				"inner.sh":  "echo from-inner\nexec 0< ./inner2.sh\necho never-either\n",
				"inner2.sh": "echo from-inner2\n",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := write(t, tc.files, "s.sh")
			want := runStdinScript(t, bash, path)
			if got := runStdinScript(t, koi, path); got != want {
				t.Errorf("koi = %q, bash = %q", got, want)
			}
		})
	}
}

// A script named on the command line is not standard input, so
// redirecting fd 0 inside it redirects only what its commands read.
// Running the same file both ways is what makes that a real distinction
// rather than an untested claim.
func TestANamedScriptKeepsItsCommandStream(t *testing.T) {
	t.Parallel()
	bash := requireBash(t)
	koi := buildKoi(t)

	path := write(t, map[string]string{
		"s.sh":     "echo one\nexec 0< ./inner.sh\nread line; echo \"read=$line\"\necho still-here\n",
		"inner.sh": "echo from-inner\n",
	}, "s.sh")
	want := runShellScript(t, bash, path, nil)
	if !strings.Contains(want, "still-here") {
		t.Fatalf("the oracle switched streams for a named script: %q", want)
	}
	if got := runShellScript(t, koi, path, nil); got != want {
		t.Errorf("koi = %q, bash = %q", got, want)
	}
}
