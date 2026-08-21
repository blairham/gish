//go:build unix

package main

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A script can signal its own jobs (#397).
//
// interp's table covers `set -m`, `fg` and `bg` case by case, and cannot
// cover this: `kill` is a koi builtin reached by renaming the call, so
// the path from `kill %1` to the interpreter's job table only exists in
// the assembled shell. The two owners of a `%jobspec` — the interactive
// process-group table and the interpreter's goroutines — are wired
// together there too, which is exactly the seam worth testing.

// bashPrefix is the `bash: line 1: ` a message carries and koi's does
// not, by decision (#120). It is normalized away so the comparison is
// about what the shell did.
var bashPrefix = regexp.MustCompile(`(?m)^[^\n]*: line [0-9]+: `)

func runJobScript(t *testing.T, shell, src string) string {
	t.Helper()
	cmd := exec.Command(shell, "-c", src)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb"}
	cmd.Stdin = nil
	out, _ := cmd.CombinedOutput() // a non-zero status is part of what is compared
	text := bashPrefix.ReplaceAllString(string(out), "")
	// bash reports a job killed by an uncatchable signal as a job
	// notice; koi has no asynchronous notices in a script.
	var kept []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "Killed:") || strings.Contains(line, "Terminated:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n") + "status=" + strconv.Itoa(cmd.ProcessState.ExitCode())
}

func TestAScriptCanSignalItsOwnJobs(t *testing.T) {
	t.Parallel()
	bash := requireBash(t)
	koi := buildKoi(t)

	for _, tc := range []struct{ name, src string }{
		// The status a signaled job reports is the signal's, which is
		// what a script reads to tell a kill from a failure.
		{"term", `sleep 5 & kill %1; wait %1; echo w=$?`},
		{"kill -9", `sleep 5 & kill -9 %1; wait %1; echo w=$?`},
		{"kill -INT", `sleep 5 & kill -INT %1; wait %1; echo w=$?`},
		{"by name", `sleep 5 & kill -TERM %% ; wait %1; echo w=$?`},
		// Signal 0 asks whether the job is there and changes nothing.
		{"signal 0", `sleep 0.2 & kill -0 %1; echo k=$?; wait`},
		// A job that has finished is still a job to name.
		{"already finished", `sleep 0.05 & wait %1; kill %1; echo k=$?`},
		// And these are not jobs at all.
		{"no such job", `sleep 0.05 & kill %2; echo k=$?; wait`},
		{"no jobs", `kill %1; echo k=$?`},
		{"disowned", `sleep 0.05 & disown; kill %1; echo k=$?; wait`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := runJobScript(t, bash, tc.src)
			if got := runJobScript(t, koi, tc.src); got != want {
				t.Errorf("koi = %q, bash = %q", got, want)
			}
		})
	}
}

// What koi cannot do it says rather than ignoring: a goroutine has no
// stopped state, so `kill -STOP %1` is refused instead of quietly doing
// nothing. bash stops the job, which is a real divergence and is why it
// is asserted here rather than compared.
func TestStoppingAScriptJobIsRefused(t *testing.T) {
	t.Parallel()
	koi := buildKoi(t)

	got := runJobScript(t, koi, `sleep 0.05 & kill -STOP %1; echo k=$?; wait`)
	if !strings.Contains(got, "cannot stop a job") {
		t.Errorf("output = %q, want a refusal naming what it cannot do", got)
	}
	if !strings.Contains(got, "k=1") {
		t.Errorf("output = %q, want a non-zero status for the refusal", got)
	}
}
