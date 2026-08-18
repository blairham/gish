//go:build unix

package main

import (
	"os/exec"
	"testing"
	"time"
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

// foregroundCmd is runningCmd with a heartbeat, for the one test that has
// to observe the child *running again* after `fg`.
//
// runningCmd prints its marker once, before it is ever stopped, so after
// `fg` there is nothing new to wait for — and waiting on `fg`'s echo of
// the resumed command is what made TestJobControlForegroundAndInterrupt
// flaky (#226). The echo is printed *before* fg has moved the child into
// the foreground process group and handed it the terminal, so a Ctrl-C
// sent on the strength of it lands in the handover window and is
// delivered to nobody: the terminal still echoes "^C", `sleep 45` keeps
// running, and the wait expires against a shell that behaved perfectly.
//
// A heartbeat makes the state observable instead of assumed. A tick
// arriving after the buffer is cleared can only have come from a child
// that is actually running again, which is the precondition Ctrl-C
// needs. It also proves the stop: no tick arrives while the job is
// stopped.
const foregroundCmd = `sh -c 'printf "res%s\n" RUNNING; while :; do sleep 1; printf "res%s\n" TICK; done'`

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
	s.stopForeground()

	// The stopped job is filed and listed.
	//
	// Each line ends with its own marker and the wait is on that marker,
	// never on the interesting output. Waiting on "Stopped" would return
	// while the line was still finishing, so the next send landed during
	// the prompt redraw and was lost — which is how this passed locally
	// and failed on the ubuntu runner.
	s.runProbe(`jobs; printf "res%s\n" J1`, "resJ1")
	if !s.seen("Stopped") {
		t.Errorf("jobs did not list the stopped job:\n%s", s.plain())
	}

	// bg resumes it, and says so in bash's shape: [id] command &
	s.runProbe(`bg; printf "res%s\n" B1`, "resB1")
	if !s.seen("[1]") {
		t.Errorf("bg did not report the resumed job:\n%s", s.plain())
	}

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

	s.send(foregroundCmd + "\r")
	s.waitFor("resRUNNING")
	s.stopForeground()

	// Cleared before the send, never after: a tick that shared a read with
	// fg's echo would be thrown away by a later reset, and the wait would
	// then sit through a whole heartbeat interval for no reason — or miss
	// it entirely if the child were stopped again first. Clear-then-send
	// is the same rule runLine follows.
	s.buf.Reset()
	s.send("fg\r")
	// Not `fg`'s echo of the command: that is printed before the handover
	// and is precisely the wait that made this test flaky (#226). A tick
	// can only come from a child that is running again.
	s.waitFor("resTICK")

	// Ctrl-C must interrupt the child and leave the shell alive.
	//
	// It cannot carry its own marker, and there is no portable signal for
	// "the process group is ready to receive it", so the honest contract
	// is to send until the effect is observed — a fresh prompt. A Ctrl-C
	// that lands in a handover window is delivered to nobody and produces
	// no prompt, so the resend is what closes the race rather than a
	// longer wait. Repeats are harmless: at a prompt, Ctrl-C discards an
	// empty line and redraws.
	s.sendUntil("\x03", promptEnd)

	s.probe("AFTERINT")
	s.waitFor("resAFTERINT")
}

// stopForeground sends Ctrl-Z and waits until the shell has both filed
// the job and finished drawing the prompt that follows.
//
// Both halves matter and they are separate events. "Stopped" is the
// notice; the prompt after it is when the shell is reading again. Typing
// on the strength of the notice alone lands in the redraw and is lost —
// the failure this file kept rediscovering, in four places, one per
// caller.
//
// Ctrl-Z is resent on silence for the same reason Ctrl-C is: it is a
// keystroke aimed at a process group mid-transition, and a lost one is
// indistinguishable from a shell that never answered. It is idempotent
// here — once the job is stopped the shell is at a prompt, where Ctrl-Z
// does nothing.
func (s *ptySession) stopForeground() {
	s.t.Helper()
	s.sendUntil("\x1a", "Stopped") // clears the buffer itself
	s.waitForPrompt()
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

	s.runProbe(`jobs; printf "res%s\n" NOJOBS`, "resNOJOBS")
	if s.seen("Stopped") || s.seen("Running") {
		t.Errorf("jobs listed something in a fresh session:\n%s", s.plain())
	}
}

// TestBackgroundCommandIsAJob covers `cmd &` end to end (#57).
//
// This was two bugs wearing one coat. The command was never started at
// all — the line's context was canceled on the way out and aborted the
// interpreter's background goroutine before it exec'd — and even once it
// ran, nothing filed it, because the line's job slot is closed by the
// time a backgrounded statement reaches the exec seam.
//
// Which of those you saw depended on timing, and timing resolved
// differently per platform, so the symptom looked like flaky
// bookkeeping. It was neither flaky nor bookkeeping.
func TestBackgroundCommandIsAJob(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.send(`sleep 45 & printf "res%s\n" BACKGROUNDED` + "\r")
	s.waitFor("resBACKGROUNDED")

	// The process runs — asked of the operating system from the test
	// process, because gish under the test pty cannot exec pgrep itself.
	if !processExists(t, "sleep 45") {
		t.Fatalf("`sleep 45 &` left no running process:\n%s", s.plain())
	}

	// And it is a job, which is the half that decides whether jobs, fg
	// and kill %n can see it at all.
	s.runProbe(`jobs; printf "res%s\n" LISTED`, "resLISTED")
	if !s.seen("sleep 45") {
		t.Errorf("jobs did not list the backgrounded command:\n%s", s.plain())
	}

	// Reachable by job spec.
	s.runProbe(`kill %1; printf "res%s\n" KILLED`, "resKILLED")
	if s.seen("no such job") {
		t.Errorf("kill %%1 could not reach the backgrounded job:\n%s", s.plain())
	}

	// The shell kept the terminal throughout: a background job must
	// never be handed it, however the spawn raced the line.
	s.buf.Reset()
	s.probe("ALIVE")
	s.waitFor("resALIVE")
}

// TestBackgroundJobDoesNotStealTheTerminal is the invariant that makes
// the whole path safe to add: every difference a background job
// introduces is a step the foreground handoff takes and it skips.
//
// A shell that handed the terminal to a background job would leave the
// user typing into a process that is not listening, which is
// indistinguishable from a hang.
func TestBackgroundJobDoesNotStealTheTerminal(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.send(`sleep 45 & printf "res%s\n" BG` + "\r")
	s.waitFor("resBG")
	processExists(t, "sleep 45")

	// The shell still reads keys and runs commands.
	for _, name := range []string{"ONE", "TWO"} {
		s.runProbe(`printf "res%s\n" `+name, "res"+name)
	}
	s.runProbe(`kill %1; printf "res%s\n" DONE`, "resDONE")
}

// Command substitution and pipelines must not become jobs. They run
// off-line too, so a mechanism keyed on anything but position would
// sweep them up — and bash does not job-control $( ) either.
func TestSubstitutionIsNotAJob(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.runProbe(`x=$(echo hi); echo a | grep -q a; jobs; printf "res%s\n" NONE`, "resNONE")
	if s.seen("Running") || s.seen("Stopped") {
		t.Errorf("a substitution or pipeline was filed as a job:\n%s", s.plain())
	}
}

// TestKillByJobSpec covers the half of kill that needs the shell: %1
// resolves through the same table jobs/fg/bg read, and signals the
// job's process *group* rather than one pid — a pipeline is several
// processes, and killing only the first leaves the rest running.
//
// Before this, kill was recognized by the interpreter and answered
// "unsupported builtin", which is worse than absent: a claimed name
// never reaches the exec seam, so the working /bin/kill was shadowed by
// something that refused to run. A job you could stop with Ctrl-Z could
// not be killed.
func TestKillByJobSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.send(runningCmd + "\r")
	s.waitFor("resRUNNING")
	s.stopForeground()

	// The assertion is that the process dies, checked by asking the
	// system rather than the shell. `jobs` is the wrong oracle here: a
	// stopped job that dies is never removed from the table (#59), which
	// is a separate, pre-existing bug — an external pkill leaves the same
	// stale entry, without gish's kill involved at all. Asserting on the
	// listing would have blamed kill for something it does not do.
	//
	// pgrep exits 0 when it finds a match and 1 when it does not, so the
	// status is the answer. The marker is split — "res%s" and "PG$?" in
	// the command, "resPG1" only in the output — because the terminal
	// echoes what is typed and a whole marker would match on the echo.
	s.runProbe(`kill %1; sleep 1; pgrep -f "sleep 45" >/dev/null; printf "res%s\n" "PG$?"`, "resPG")
	if !s.seen("resPG1") {
		t.Errorf("`kill %%1` left the job running:\n%s", s.plain())
	}

	// And the table notices. This is the half that was broken (#59):
	// nothing owned the wait for a stopped job, so the process became a
	// zombie and the entry outlived it.
	s.runProbe(`jobs; printf "res%s\n" LIST`, "resLIST")
	if s.seen("sleep 45") {
		t.Errorf("jobs still lists a job whose process is gone:\n%s", s.plain())
	}
}

// TestStoppedJobKilledExternallyIsReaped is the same invariant without
// gish's kill involved at all.
//
// It is the test that told the two bugs apart: when `kill %1` left a
// stale entry, an external pkill left exactly the same one, which said
// the fault was in the table rather than in signaling. Keeping it means
// a future regression is attributed correctly the first time.
func TestStoppedJobKilledExternallyIsReaped(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.send(runningCmd + "\r")
	s.waitFor("resRUNNING")
	s.stopForeground()

	s.runProbe(`pkill -KILL -f "sleep 45"; sleep 1; printf "res%s\n" KILLED`, "resKILLED")

	s.runProbe(`jobs; printf "res%s\n" LIST`, "resLIST")
	if s.seen("sleep 45") {
		t.Errorf("a stopped job killed from outside was never reaped:\n%s", s.plain())
	}
}

// TestKillRejectsUnknownJobSpec: a spec naming nothing must say so,
// rather than silently succeeding and leaving the caller believing
// something was signaled.
func TestKillRejectsUnknownJobSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	s := startPTY(t, ptyOptions{})
	s.waitForPrompt()

	s.runProbe(`kill %99; printf "res%s\n" NOJOB`, "resNOJOB")
	if !s.seen("no such job") {
		t.Errorf("kill %%99 did not report a missing job:\n%s", s.plain())
	}
}

// processExists polls the system for a process whose command line
// matches pattern, from the test process rather than through the shell.
//
// Polled because `cmd &` returns before the child exists: the
// interpreter spawns it on its own goroutine, so a marker printed by the
// same line proves the line ran, not that the process is there yet.
// Bounded, so a genuine absence fails rather than hangs.
func processExists(t *testing.T, pattern string) bool {
	t.Helper()
	for range 20 {
		if err := exec.Command("pgrep", "-f", pattern).Run(); err == nil {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}
