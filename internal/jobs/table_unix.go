//go:build unix

package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/capture"
)

// Supported reports whether job control is available on this platform.
func Supported() bool { return true }

// Job is one process group born from one interactive command line.
//
// Lock ordering rule for the package: a goroutine may hold job.mu OR
// Table.mu, never both. Cross-structure steps (filing, removing) release
// one lock before taking the other.
type Job struct {
	ID      int
	Pgid    int
	Command string

	mu      sync.Mutex
	cond    *sync.Cond
	state   State
	live    int  // spawned processes not yet reaped
	exit    int  // last observed exit status
	reaping bool // a reaper goroutine owns Wait4(-pgid)
}

func newJob(cmdline string) *Job {
	j := &Job{Command: cmdline}
	j.cond = sync.NewCond(&j.mu)
	return j
}

func (j *Job) snapshot() (State, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state, j.exit
}

// waitChange blocks until the job leaves Running.
func (j *Job) waitChange() (State, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for j.state == Running {
		j.cond.Wait()
	}
	return j.state, j.exit
}

// Table tracks jobs for one interactive session. tty may be nil (tests,
// no terminal): terminal handoff is skipped but grouping, stop tracking,
// and fg/bg still work.
type Table struct {
	mu        sync.Mutex
	tty       *os.File
	shellPgid int
	jobs      map[int]*Job
	nextID    int
	current   *Job

	// Output capture (#99 stage 2), off unless enabled. It lives here
	// rather than in the repl because this is the only place a
	// foreground child's stdio is chosen, and capture is exactly a
	// substitution of that stdio.
	captureLimit int
	capturing    *capture.Session
	lastCapture  []byte
	lastTrunc    bool
}

// NewTable creates the session job table. tty is the controlling
// terminal used for foreground handoff (nil to disable terminal ops).
func NewTable(tty *os.File) *Table {
	return &Table{
		tty:       tty,
		shellPgid: unix.Getpgrp(),
		jobs:      map[int]*Job{},
		nextID:    1,
	}
}

// EnableCapture turns on per-line output capture with a byte cap.
// A zero or negative limit uses the package default; capture stays off
// until this is called.
func (t *Table) EnableCapture(limit int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if limit <= 0 {
		limit = capture.DefaultLimit
	}
	t.captureLimit = limit
}

// BeginLine opens a job slot for the next foreground command line, and
// with capture enabled, the session that line's output flows through.
func (t *Table) BeginLine(cmdline string) {
	t.mu.Lock()
	t.current = newJob(cmdline)
	t.lastCapture, t.lastTrunc = nil, false
	limit, tty := t.captureLimit, t.tty
	t.mu.Unlock()

	if limit <= 0 {
		return
	}
	// A missing tty is not a reason to skip capture — it only means the
	// size has to be guessed. Substitution still depends on the child's
	// stdout actually being a terminal, which is checked at spawn.
	cols, rows := terminalSize(tty)
	sess, err := capture.Start(os.Stdout, cols, rows, limit)
	if err != nil {
		// A shell that cannot allocate a pty still has to run commands.
		// Capture is the optional half.
		return
	}
	t.mu.Lock()
	t.capturing = sess
	t.mu.Unlock()
}

// LastCapture returns the previous line's output and whether it was
// truncated. Empty when capture is off or the line produced nothing.
func (t *Table) LastCapture() (out []byte, truncated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastCapture, t.lastTrunc
}

// Resize keeps a live capture's pty matching the real terminal, so a
// program reading its width from stdout is not told a stale one.
func (t *Table) Resize(cols, rows int) {
	t.mu.Lock()
	sess := t.capturing
	t.mu.Unlock()
	if sess != nil {
		sess.Resize(cols, rows)
	}
}

// terminalSize reads the real terminal's dimensions, falling back to a
// conventional size rather than zero — a zero-width terminal makes
// well-behaved programs behave strangely.
func terminalSize(tty *os.File) (cols, rows int) {
	if tty != nil {
		if ws, err := pty.GetsizeFull(tty); err == nil && ws.Cols > 0 && ws.Rows > 0 {
			return int(ws.Cols), int(ws.Rows)
		}
	}
	return 80, 24
}

// EndLine closes out the foreground line: the shell takes the terminal
// back, and the job is filed if it stopped (Ctrl-Z) or still has live
// processes (background &). The Notice tells the shell what to print.
func (t *Table) EndLine() (Notice, bool) {
	t.mu.Lock()
	job := t.current
	t.current = nil
	sess := t.capturing
	t.capturing = nil
	t.mu.Unlock()
	if sess != nil {
		out, trunc := sess.Close(), sess.Truncated()
		t.mu.Lock()
		t.lastCapture, t.lastTrunc = out, trunc
		t.mu.Unlock()
	}
	t.takeTerminal()
	if job == nil || job.Pgid == 0 {
		return Notice{}, false // no external process was spawned
	}
	job.mu.Lock()
	keep := job.state == Stopped || job.live > 0
	stopped := job.state == Stopped
	job.mu.Unlock()
	if !keep {
		return Notice{}, false
	}
	t.file(job)
	return Notice{ID: job.ID, Command: job.Command, Stopped: stopped}, true
}

func (t *Table) file(job *Job) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if job.ID == 0 {
		job.ID = t.nextID
		t.nextID++
		t.jobs[job.ID] = job
	}
}

func (t *Table) remove(job *Job) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if job.ID != 0 {
		delete(t.jobs, job.ID)
	}
}

// --- terminal handoff ---

func (t *Table) takeTerminal()         { t.setForeground(t.shellPgid) }
func (t *Table) giveTerminal(pgid int) { t.setForeground(pgid) }

// setForeground is best-effort: the shell ignores SIGTTOU for the
// session, so reclaiming from the background cannot stop it.
func (t *Table) setForeground(pgid int) {
	if t.tty == nil {
		return
	}
	_ = unix.IoctlSetPointerInt(int(t.tty.Fd()), unix.TIOCSPGRP, pgid) //nolint:errcheck // best-effort
}

// --- spawning ---

// ExecMiddleware places external commands of the current foreground line
// into the line's process group and waits with WUNTRACED so stops are
// observed. Non-file stdio (command substitution) and spawns outside a
// foreground line fall through to the next handler — same as bash not
// job-controlling $( ).
func (t *Table) ExecMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		stdout, ook := hc.Stdout.(*os.File)
		stderr, eok := hc.Stderr.(*os.File)
		// nil stdin is fine (exec maps it to /dev/null); a non-file
		// stdin means an interpreter-managed pipe — not ours to manage.
		stdin, iok := hc.Stdin.(*os.File)
		if hc.Stdin == nil {
			stdin, iok = nil, true
		}
		if !iok || !ook || !eok {
			return next(ctx, args)
		}
		t.mu.Lock()
		job := t.current
		t.mu.Unlock()
		if job == nil {
			return next(ctx, args)
		}
		return t.spawn(hc, job, args, stdin, stdout, stderr)
	}
}

func (t *Table) spawn(hc interp.HandlerContext, job *Job, args []string, stdin, stdout, stderr *os.File) error {
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.ExitStatus(127)
	}

	// The leader is decided under job.mu so concurrent pipeline stages
	// serialize: the first Start creates the group (and takes the
	// terminal in the child, pre-exec, via Foreground — race-free),
	// later stages join it.
	// Output capture (#99): swap the child's stdout for the capture pty.
	//
	// Its controlling terminal is untouched, which is the whole reason
	// this is safe: Ctrl-C, Ctrl-Z and SIGWINCH still come from the real
	// terminal to the child's process group exactly as before, so job
	// control keeps working without being reimplemented.
	//
	// **stderr is deliberately left alone**, and this is not an
	// oversight — it was measured. Full-screen programs do their
	// terminal control (tcgetattr/tcsetattr, raw mode) on **fd 2**,
	// which is standard practice precisely because stdout is so often
	// redirected. Hand a program a pty as stderr and it configures the
	// pty instead of the real terminal: `less` paints a screen, never
	// enters raw mode, and every keystroke meant for it is echoed onto
	// the shell's line instead. Verified both ways — with stderr
	// captured `less` wedges, with it left alone it behaves exactly as
	// it does uncaptured.
	//
	// The cost is real and worth stating: a command's *errors* are not
	// captured. That is the price of "programs behave exactly as they do
	// today", which is the bar this feature has to clear to be worth
	// having at all.
	//
	// Substitution is skipped when stdout is not a terminal — `cmd >
	// file` or a pipeline stage already has somewhere to go, and routing
	// it through a pty would both capture what the user sent elsewhere
	// and mangle it (the line discipline translates newlines).
	t.mu.Lock()
	sess := t.capturing
	t.mu.Unlock()
	if sess != nil && isTerminal(stdout) {
		stdout = sess.Slave()
	}

	job.mu.Lock()
	attr := &syscall.SysProcAttr{Setpgid: true}
	leader := job.Pgid == 0
	switch {
	case leader && t.tty != nil:
		attr.Foreground = true
		attr.Ctty = int(t.tty.Fd())
	case !leader:
		attr.Pgid = job.Pgid
	}
	cmd := exec.Cmd{
		Path:        path,
		Args:        args,
		Env:         execEnv(hc.Env),
		Dir:         hc.Dir,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		SysProcAttr: attr,
	}
	// Note: no context-kill hook here, deliberately. Foreground children
	// get their signals from the terminal (they own it); background jobs
	// must not die because the line's context ended (#3's cancel).
	if err := cmd.Start(); err != nil {
		job.mu.Unlock()
		fmt.Fprintf(hc.Stderr, "%v\n", err)
		return interp.ExitStatus(127)
	}
	pid := cmd.Process.Pid
	if leader {
		job.Pgid = pid
	}
	job.live++
	job.mu.Unlock()

	status := t.waitProc(job, pid)
	_ = cmd.Process.Release() // reaped via Wait4; free the handle without cmd.Wait
	return status
}

// isStopped reports a stop for statuses from Wait4 without WCONTINUED.
// On BSD/darwin, x/sys encodes "continued" as the stop marker plus
// SIGSTOP, so Stopped() alone misses SIGSTOP-stopped children; since we
// never request WCONTINUED, any stop-marked status here is a real stop.
func isStopped(ws unix.WaitStatus) bool {
	return ws.Stopped() || ws.Continued()
}

// stopExit maps a stop to the 128+signal shell convention, tolerating
// the darwin StopSignal()==-1 case for SIGSTOP.
func stopExit(ws unix.WaitStatus) int {
	if sig := ws.StopSignal(); sig > 0 {
		return 128 + int(sig)
	}
	return 128 + int(unix.SIGSTOP)
}

// waitProc waits for one spawned process, observing stops. Safe to
// bypass exec.Cmd.Wait because stdio are real files — the Cmd created no
// pipes or copy goroutines.
func (t *Table) waitProc(job *Job, pid int) error {
	for {
		var ws unix.WaitStatus
		_, err := unix.Wait4(pid, &ws, unix.WUNTRACED, nil)
		switch {
		case err == unix.EINTR:
			continue
		case err != nil:
			// Reaped elsewhere (shouldn't happen while handlers own the
			// wait); account for it so the job can finish.
			t.procDone(job, 1)
			return interp.ExitStatus(1)
		case isStopped(ws):
			job.mu.Lock()
			job.state = Stopped
			job.cond.Broadcast()
			job.mu.Unlock()
			// The whole group is stopping; the shell needs the terminal
			// to show a prompt again.
			t.takeTerminal()
			return interp.ExitStatus(stopExit(ws))
		case ws.Signaled():
			status := 128 + int(ws.Signal())
			t.procDone(job, status)
			return interp.ExitStatus(status)
		case ws.Exited():
			status := ws.ExitStatus()
			t.procDone(job, status)
			if status == 0 {
				return nil
			}
			return interp.ExitStatus(status)
		default:
			continue
		}
	}
}

// procDone records one reaped process; the job is Done (and removed from
// the table if filed) when none remain. Removal happens inside the same
// job.mu section that flips the state: a waiter woken by (or arriving
// after) the Done transition must never find the entry still listed —
// `fg` returning and `jobs` showing the job would race otherwise. Taking
// t.mu under job.mu is safe: no path acquires them in the other order.
func (t *Table) procDone(job *Job, exit int) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.live--
	job.exit = exit
	if job.live <= 0 {
		job.state = Done
		job.reaping = false
		t.remove(job)
		job.cond.Broadcast()
	}
}

// ensureReaper takes over Wait4(-pgid) ownership after the per-process
// handlers have returned — which is guaranteed once a job is Stopped.
func (t *Table) ensureReaper(job *Job) {
	job.mu.Lock()
	start := !job.reaping && job.live > 0
	if start {
		job.reaping = true
	}
	job.mu.Unlock()
	if start {
		go t.reap(job)
	}
}

func (t *Table) reap(job *Job) {
	for {
		var ws unix.WaitStatus
		_, err := unix.Wait4(-job.Pgid, &ws, unix.WUNTRACED, nil)
		switch {
		case err == unix.EINTR:
			continue
		case err != nil: // ECHILD: nothing left in the group
			job.mu.Lock()
			job.live = 0
			job.state = Done
			job.reaping = false
			t.remove(job) // before the broadcast, same as procDone
			job.cond.Broadcast()
			job.mu.Unlock()
			return
		case isStopped(ws):
			job.mu.Lock()
			job.state = Stopped
			job.cond.Broadcast()
			job.mu.Unlock()
		case ws.Signaled():
			t.procDone(job, 128+int(ws.Signal()))
			if job.doneNow() {
				return
			}
		case ws.Exited():
			t.procDone(job, ws.ExitStatus())
			if job.doneNow() {
				return
			}
		}
	}
}

func (j *Job) doneNow() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state == Done
}

// pick resolves a job argument ("%1" or "1"), defaulting to the most
// recently filed stopped job, then the most recent job.
func (t *Table) pick(args []string) *Job {
	t.mu.Lock()
	if len(args) > 0 {
		id, err := strconv.Atoi(strings.TrimPrefix(args[0], "%"))
		job := t.jobs[id]
		t.mu.Unlock()
		if err != nil {
			return nil
		}
		return job
	}
	list := make([]*Job, 0, len(t.jobs))
	for _, j := range t.jobs {
		list = append(list, j)
	}
	t.mu.Unlock()

	slices.SortFunc(list, func(a, b *Job) int { return b.ID - a.ID }) // newest first
	for _, j := range list {
		if s, _ := j.snapshot(); s == Stopped {
			return j
		}
	}
	if len(list) > 0 {
		return list[0]
	}
	return nil
}

// --- builtins (registered by the shell as __gish_jobs/fg/bg) ---

// Jobs lists the table.
func (t *Table) Jobs(_ context.Context, hc interp.HandlerContext, _ []string) error {
	t.mu.Lock()
	list := make([]*Job, 0, len(t.jobs))
	for _, j := range t.jobs {
		list = append(list, j)
	}
	t.mu.Unlock()
	slices.SortFunc(list, func(a, b *Job) int { return a.ID - b.ID })
	for _, j := range list {
		state, _ := j.snapshot()
		fmt.Fprintf(hc.Stdout, "[%d]  %-8s %s\n", j.ID, state, j.Command)
	}
	return nil
}

// Fg resumes a job in the foreground and waits for it to stop or finish.
func (t *Table) Fg(_ context.Context, hc interp.HandlerContext, args []string) error {
	job := t.pick(args)
	if job == nil {
		fmt.Fprintln(hc.Stderr, "fg: no current job")
		return interp.ExitStatus(1)
	}
	fmt.Fprintln(hc.Stdout, job.Command)
	wasStopped := t.resume(job, true)
	_ = wasStopped
	state, exit := job.waitChange()
	t.takeTerminal()
	if state == Stopped {
		fmt.Fprintf(hc.Stdout, "[%d]  Stopped  %s\n", job.ID, job.Command)
		return interp.ExitStatus(148)
	}
	if exit == 0 {
		return nil
	}
	return interp.ExitStatus(exit)
}

// Bg resumes a stopped job in the background.
func (t *Table) Bg(_ context.Context, hc interp.HandlerContext, args []string) error {
	job := t.pick(args)
	if job == nil {
		fmt.Fprintln(hc.Stderr, "bg: no current job")
		return interp.ExitStatus(1)
	}
	if state, _ := job.snapshot(); state != Stopped {
		fmt.Fprintf(hc.Stderr, "bg: job %d already running\n", job.ID)
		return interp.ExitStatus(1)
	}
	t.resume(job, false)
	fmt.Fprintf(hc.Stdout, "[%d]  %s &\n", job.ID, job.Command)
	return nil
}

// resume continues a job's group; foreground also hands it the terminal.
// A reaper is started only when the job was stopped — that is the state
// that guarantees the original per-process handlers have returned, so
// wait ownership is free to take.
func (t *Table) resume(job *Job, foreground bool) (wasStopped bool) {
	state, _ := job.snapshot()
	wasStopped = state == Stopped
	if foreground {
		t.giveTerminal(job.Pgid)
	}
	_ = unix.Kill(-job.Pgid, unix.SIGCONT) //nolint:errcheck // group may have just exited
	job.mu.Lock()
	if job.state == Stopped {
		job.state = Running
	}
	job.mu.Unlock()
	if wasStopped {
		t.ensureReaper(job)
	}
	return wasStopped
}

// execEnv mirrors the interpreter's exported-variable environment
// construction (interp.execEnv is unexported).
func execEnv(env expand.Environ) []string {
	list := make([]string, 0, 64)
	env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.Kind == expand.String {
			list = append(list, name+"="+vr.String())
		}
		return true
	})
	return list
}

// Count reports filed jobs (running or stopped), for prompt display.
func (t *Table) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.jobs)
}

// Commands returns the command line of every tracked job, for session
// recording (#103). The processes themselves do not survive a restart;
// their command lines do, as re-runnable text.
func (t *Table) Commands() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.jobs))
	for _, j := range t.jobs {
		if j.Command != "" {
			out = append(out, j.Command)
		}
	}
	return out
}

// isTerminal reports whether f is a terminal, the test for whether
// capture should stand in for it. x/term rather than a raw ioctl: the
// termios request differs between Linux and the BSDs, and this file
// builds for both.
func isTerminal(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

// DisableCapture turns per-line capture back off. A line already in
// flight keeps its session; the next one runs uncaptured.
func (t *Table) DisableCapture() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.captureLimit = 0
}
