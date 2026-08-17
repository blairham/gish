//go:build unix

package main

import (
	"testing"
)

// Job control end to end (#55, #5).
//
// jobs/fg/bg had no end-to-end coverage at all. They cannot have any
// other kind: they exist only in an interactive session, they are
// reached through the CallHandler rewrite rather than the exec seam, and
// what they report depends on process groups and terminal ownership —
// none of which a unit test with a fake terminal can produce. Every
// assertion here needed a real pty to be worth anything.
//
// The commands under test print a marker before sleeping. A bare `sleep`
// produces no output, so there is no way to know it is actually running
// rather than about to be; waiting on the marker is the difference
// between testing job control and testing a timer.

// runningCmd is one external child that prints a marker and then sleeps
// long enough for the test to drive every transition.
//
// Two rules, each learned by breaking it here:
//
// It must be a single external child, and the marker must come from that
// child. Written as `printf 'resRUNNING\n'; sleep 45`, printf is a gish
// builtin, so the marker appeared before sleep had spawned and the
// Ctrl-Z that followed found no foreground child to stop. Waiting on
// output proves output happened, not that the next thing is ready.
//
// And the marker must not exist in the command text, because the
// terminal echoes what is typed: `echo resRUNNING` matches on the echo,
// so the wait succeeded before the line was even accepted. This is the
// same split-sentinel trick probe() uses — "res%s" and "RUNNING" appear
// separately in the command, and "resRUNNING" only in the output.
const runningCmd = `sh -c 'printf "res%s\n" RUNNING; sleep 45'`

// TestJobControlStopListResume walks the flow that makes job control
// worth having: interrupt what you are running, look at it, put it back.
func TestJobControlStopListResume(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.send(runningCmd + "\r")
	s.waitFor("resRUNNING") // proof the child is live, not merely spawned

	// Ctrl-Z is a raw keystroke: only a shell that owns the terminal and
	// put the child in its own process group turns it into SIGTSTP.
	s.buf.Reset()
	s.send("\x1a")
	s.waitFor("Stopped")

	// The stopped job is filed and listed.
	s.buf.Reset()
	s.send("jobs\r")
	s.waitFor("Stopped")

	// bg resumes it, and says so in bash's shape: [id] command &
	s.buf.Reset()
	s.send("bg\r")
	s.waitFor("[1]")

	// The shell is still usable afterwards, which is the part that breaks
	// when terminal ownership is handed back wrongly.
	s.probe("AFTERBG")
	s.waitFor("resAFTERBG")
}

// TestJobControlForegroundAndInterrupt covers the other half: fg takes
// the job back, and Ctrl-C then reaches the child rather than the shell.
//
// The shell surviving its own SIGINT is the #3 posture, and this is the
// path where a mistake in it would be invisible to every other test —
// the signal has to arrive at a process group the shell handed the
// terminal to and then took back.
func TestJobControlForegroundAndInterrupt(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.send(runningCmd + "\r")
	s.waitFor("resRUNNING")
	s.buf.Reset()
	s.send("\x1a")
	s.waitFor("Stopped")

	s.buf.Reset()
	s.send("fg\r")
	// fg echoes the command it is resuming.
	s.waitFor("sleep 45")

	// Ctrl-C must interrupt the child and leave the shell alive.
	s.buf.Reset()
	s.send("\x03")
	s.probe("AFTERINT")
	s.waitFor("resAFTERINT")
}

// TestJobsIsEmptyWhenNothingRuns: the listing has to be able to say
// "nothing", or a stale entry would be indistinguishable from a real
// one.
func TestJobsEmptyListing(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.buf.Reset()
	s.send("jobs\r")
	s.probe("NOJOBS")
	s.waitFor("resNOJOBS")
	if s.seen("Stopped") || s.seen("Running") {
		t.Errorf("jobs listed something in a fresh session:\n%s", s.plain())
	}
}

// TestBackgroundAmpersandIsNotFiled records a real gap, deliberately.
//
// `cmd &` creates no job: nothing is announced, `jobs` cannot list it,
// and `fg` cannot reach it. Ctrl-Z works — that path files through
// EndLine, which keeps a job that stopped or still has live processes —
// but a command backgrounded with & spawns on the interpreter's own
// goroutine, and by the time it reaches the exec handler the line's job
// slot is already closed, so it runs untracked.
//
// Asserted as it stands rather than skipped, so the day it is fixed this
// test fails and gets replaced by the real one. A skip would go quiet
// forever; deleting the case would lose the finding entirely.
func TestBackgroundAmpersandIsNotFiled(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.send("sleep 45 &\r")
	s.probe("BACKGROUNDED")
	s.waitFor("resBACKGROUNDED")

	s.buf.Reset()
	s.send("jobs\r")
	s.probe("LISTED")
	s.waitFor("resLISTED")

	if s.seen("sleep 45") {
		t.Error("`cmd &` now files a job — the gap closed. " +
			"Replace this with the real assertions: the shell should announce [id] pid, " +
			"`jobs` should list it, and `fg` should reach it.")
	}
}
