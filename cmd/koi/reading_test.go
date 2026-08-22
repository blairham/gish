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

// `shopt -s extglob` changes how the rest of the script is *parsed*
// (#619): with the option off, `echo +(a|b)c` is a syntax error at the
// `(` rather than a pattern that declines to match. So the option a line
// sets has to reach the parser before the next line is read, which is the
// same seam #450 built for posix mode — and interp's own table cannot see
// it, because that table parses a whole script and then runs it.
func TestExtGlobChangesHowLaterLinesAreRead(t *testing.T) {
	t.Parallel()
	bash := requireBash(t)
	koi := buildKoi(t)

	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{
			// extglob.tests' own shape: the option on the first line,
			// then two hundred lines of word-position patterns.
			"a later line",
			map[string]string{"s.sh": "shopt -s extglob\n>abc\necho +(a|b)c\necho after\n"},
		},
		{
			// The line the issue was filed on: an empty pattern list is
			// legal syntax with the option on, and matches nothing, so
			// the word survives as itself.
			"an empty group is literal",
			map[string]string{"s.sh": "shopt -s extglob\necho +()c\necho after\n"},
		},
		{
			// With the option off, `@()` is how a function named `@` is
			// defined — so this runs only if the option really went back
			// off, which makes it the reverse direction rather than a
			// second reading of the same one.
			"turned back off",
			map[string]string{"s.sh": "shopt -s extglob\nshopt -u extglob\n@() { echo hi; }\n@\necho after\n"},
		},
		{
			// A case pattern is the same parsing question as a word.
			"a case pattern",
			map[string]string{"s.sh": "shopt -s extglob\ncase abc in +(a|b)c) echo yes;; *) echo no;; esac\n"},
		},
		{
			// A sourced file is read by the interpreter rather than by
			// the shell around it, so it needs the same treatment.
			"inside a sourced file",
			map[string]string{
				"s.sh":   ". ./lib.sh\necho after\n",
				"lib.sh": "shopt -s extglob\n>abc\necho +(a|b)c\n",
			},
		},
		{
			// And set before the source, it reaches the sourced file.
			"before a source",
			map[string]string{
				"s.sh":   "shopt -s extglob\n>abc\n. ./lib.sh\n",
				"lib.sh": "echo +(a|b)c\n",
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

// The same file read as standard input rather than named on the command
// line, since that is a different reader (#516, #599) with the same rule.
func TestExtGlobReachesLaterLinesOnStandardInput(t *testing.T) {
	t.Parallel()
	bash := requireBash(t)
	koi := buildKoi(t)

	path := write(t, map[string]string{
		"s.sh": "shopt -s extglob\n>abc\necho +(a|b)c\necho after\n",
	}, "s.sh")
	want := runStdinScript(t, bash, path)
	if !strings.Contains(want, "abc") {
		t.Fatalf("the oracle did not glob the pattern: %q", want)
	}
	if got := runStdinScript(t, koi, path); got != want {
		t.Errorf("koi = %q, bash = %q", got, want)
	}
}

// The option reaches the next line, not the rest of its own: bash reads a
// whole line before running any of it. Both shells fail here, and the
// message is koi's own (#120), so the status is what is compared.
func TestExtGlobDoesNotChangeItsOwnLine(t *testing.T) {
	t.Parallel()
	bash := requireBash(t)
	koi := buildKoi(t)

	for _, tc := range []struct {
		name, body string
	}{
		{
			"the same line as the shopt",
			"shopt -s extglob; echo +(a|b)c\necho after\n",
		},
		{
			// Nothing turned it on at all, which is the case that fails
			// if the option is simply left on for everyone.
			"never turned on",
			"echo +(a|b)c\necho after\n",
		},
		{
			// And turning it back off must reach the rest of the script,
			// or `shopt -u` is a request koi accepts and ignores.
			"turned back off",
			"shopt -s extglob\nshopt -u extglob\necho +(a|b)c\necho after\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := write(t, map[string]string{"s.sh": tc.body}, "s.sh")
			bashOut := runShellScript(t, bash, path, nil)
			koiOut := runShellScript(t, koi, path, nil)
			if !strings.HasSuffix(bashOut, "status=2\n") {
				t.Fatalf("the oracle did not reject the line: %q", bashOut)
			}
			if !strings.HasSuffix(koiOut, "status=2\n") {
				t.Errorf("extglob reached a line it should not have: %q", koiOut)
			}
			if strings.Contains(koiOut, "after") {
				t.Errorf("koi ran past the syntax error: %q", koiOut)
			}
		})
	}
}

// `-O extglob` on the command line and BASHOPTS in the environment both
// set the option before the first line is read, so the first line can
// already use a group. They are the only ways to ask for that.
func TestExtGlobSetBeforeTheFirstLine(t *testing.T) {
	t.Parallel()
	bash := requireBash(t)
	koi := buildKoi(t)

	run := func(shell, path string, args []string, extraEnv ...string) string {
		t.Helper()
		cmd := exec.Command(shell, append(args, path)...)
		cmd.Dir = filepath.Dir(path)
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb"}, extraEnv...)
		out, _ := cmd.CombinedOutput()
		return string(out) + "status=" + strconv.Itoa(cmd.ProcessState.ExitCode()) + "\n"
	}
	for _, tc := range []struct {
		name string
		args []string
		env  []string
	}{
		{"-O extglob", []string{"-O", "extglob"}, nil},
		{"BASHOPTS", nil, []string{"BASHOPTS=extglob"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := write(t, map[string]string{
				"s.sh": ">abc\necho +(a|b)c\n",
			}, "s.sh")
			want := run(bash, path, tc.args, tc.env...)
			if !strings.Contains(want, "abc") {
				t.Fatalf("the oracle did not set the option: %q", want)
			}
			if got := run(koi, path, tc.args, tc.env...); got != want {
				t.Errorf("koi = %q, bash = %q", got, want)
			}
		})
	}
}

// `-c` reads line by line too, which is easy to assume it does not: bash
// parses the whole string no more than it parses a whole file, so an
// option set on the string's first line reaches its second. One option on
// [interp.Runner.ParserOptions] is what makes every reader agree, and
// this is the one that is not a file.
//
// `eval` is the reader that still does not, and deliberately not fixed
// here: it parses its string with the parser's own defaults, so neither
// this option nor posix mode reaches it, and giving it the options
// without also making it read incrementally would break the shape that
// works today (#682).
func TestExtGlobReachesLaterLinesOfACommandString(t *testing.T) {
	t.Parallel()
	bash := requireBash(t)
	koi := buildKoi(t)

	const script = "shopt -s extglob\n>abc\necho +(a|b)c\n"
	run := func(shell string) string {
		t.Helper()
		cmd := exec.Command(shell, "-c", script)
		cmd.Dir = t.TempDir()
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb"}
		out, _ := cmd.CombinedOutput()
		return string(out) + "status=" + strconv.Itoa(cmd.ProcessState.ExitCode()) + "\n"
	}
	want := run(bash)
	if !strings.Contains(want, "abc") {
		t.Fatalf("the oracle did not glob the pattern: %q", want)
	}
	if got := run(koi); got != want {
		t.Errorf("koi = %q, bash = %q", got, want)
	}
}
