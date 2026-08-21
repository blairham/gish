// Copyright (c) 2026, Blair Hamilton. See LICENSE for licensing information.

package interp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"syscall"
)

// The `jobs` listing format, measured against bash 5.3 rather than inferred:
//
//	[1]-  Running                    sleep 5 &
//	[2]+  Done                       { :; }
//	[1]+ 60775 Running                    sleep 5 &      <- jobs -l
//
// The status sits in a 27-column left-aligned field starting at column 7, so
// the command always begins at column 34. `jobs -l` spends that gap on the pid
// instead. A *running* job carries a trailing " &" and a finished one does not,
// which is a rule of bash's rendering rather than of what was typed -- the same
// `{ sleep 5; } &` prints with the ampersand while running and without it once
// it is done.
const (
	jobStatusCol   = 27
	jobStatusRun   = "Running"
	jobStatusDone  = "Done"
	jobStatusStop  = "Stopped"
	jobMarkCurrent = "+"
	jobMarkPrev    = "-"
	jobMarkNone    = " "
)

// jobsBuiltin implements `jobs` (#302).
//
// Refusing it was worse than it looks. The bounded parallel loop everyone
// writes counts with it:
//
//	while (( $(jobs -r | wc -l) >= MAX )); do wait -n; done
//
// and a `jobs` that fails leaves `wc -l` counting zero, so the `while` never
// blocks and a script that asked for at most MAX in flight silently runs all of
// them at once. The bound is not reported missing, it is just absent -- which
// is why this is a correctness bug rather than a missing feature.
func (r *Runner) jobsBuiltin(args []string) exitStatus {
	var (
		exit             exitStatus
		pidsOnly, longFm bool
		onlyRunning      bool
		onlyStopped      bool
	)
	failf := func(code uint8, format string, a ...any) exitStatus {
		r.errf(format, a...)
		exit.code = code
		return exit
	}
	fp := flagParser{remaining: args}
	for fp.more() {
		switch flag := fp.flag(); flag {
		case "-p":
			pidsOnly = true
		case "-l":
			longFm = true
		case "-r":
			onlyRunning = true
		case "-s":
			onlyStopped = true
		case "-n":
			// bash lists only jobs whose status changed since the last
			// report. koi has no job control and so no notifications to
			// be behind on: every job it can list is one the shell has
			// not reported yet, which makes -n the same list as the
			// default one rather than a silently empty one.
		default:
			return failf(2, "jobs: %q: invalid option\n", flag)
		}
	}

	specs := fp.args()
	idxs, err := r.jobSelection(specs)
	if err != nil {
		return failf(1, "%s", err.Error())
	}

	for _, i := range idxs {
		job := &r.bgProcs[i]
		running := !job.finished()
		switch {
		case job.disowned:
			// `disown` forgot it, so it is not in the table any more
			// (#397).
			continue
		case onlyRunning && !running:
			continue
		case !running && job.reported && len(specs) == 0:
			// Already mentioned once. bash drops a finished job from
			// the table as soon as it has reported it, so a second
			// `jobs` does not announce the same completion again --
			// but naming the job explicitly still answers.
			continue
		case onlyStopped:
			// koi has no SIGTSTP and no job control, so nothing is ever
			// stopped. Answering with an empty list is the truthful
			// answer here, not a stub: there genuinely are no stopped
			// jobs to report.
			continue
		}
		if !running {
			job.reported = true
		}
		if pidsOnly {
			r.outf("%s\n", bgPID(i))
			continue
		}
		r.out(r.jobLine(i, longFm))
	}
	return exit
}

// jobLine renders one job the way bash does; see the constants above.
func (r *Runner) jobLine(i int, long bool) string {
	job := &r.bgProcs[i]
	status := jobStatusDone
	cmd := job.cmd
	switch {
	case !job.finished():
		status = jobStatusRun
		cmd += " &"
	case job.exit.code != 0:
		// bash names the status a finished job answered with, and only
		// calls it "Done" when that was zero. A signalled job is spelled
		// per platform there (`Terminated: 15` on darwin, `Terminated`
		// on linux); koi reports the 128+n it stored, which is portable
		// and true, rather than picking one of the two.
		status = fmt.Sprintf("Exit %d", job.exit.code)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%d]%s", i+1, r.jobMark(i))
	if long {
		fmt.Fprintf(&sb, " %s ", bgPID(i))
	} else {
		sb.WriteString("  ")
	}
	fmt.Fprintf(&sb, "%-*s%s\n", jobStatusCol, status, cmd)
	return sb.String()
}

// jobMark is bash's "+" for the current job and "-" for the previous one,
// which is what a bare "%%" and "%-" refer to.
func (r *Runner) jobMark(i int) string {
	switch i {
	case len(r.bgProcs) - 1:
		return jobMarkCurrent
	case len(r.bgProcs) - 2:
		return jobMarkPrev
	}
	return jobMarkNone
}

// jobSelection resolves the operands to job indices, defaulting to every job
// the shell knows about.
func (r *Runner) jobSelection(specs []string) ([]int, error) {
	if len(specs) == 0 {
		idxs := make([]int, len(r.bgProcs))
		for i := range idxs {
			idxs[i] = i
		}
		return idxs, nil
	}
	idxs := make([]int, 0, len(specs))
	for _, spec := range specs {
		i, ok := r.jobIndex(spec)
		if !ok {
			return nil, fmt.Errorf("jobs: %s: no such job\n", spec)
		}
		idxs = append(idxs, i)
	}
	return idxs, nil
}

// jobIndex resolves one job spec. Both spellings are accepted because both
// reach this builtin in practice: "%1" is what a person writes, and "g1" is
// what $! hands back, so `jobs "$!"` has to work too.
func (r *Runner) jobIndex(spec string) (int, bool) {
	if rest, ok := strings.CutPrefix(spec, "%"); ok {
		switch rest {
		case "%", "+":
			return len(r.bgProcs) - 1, len(r.bgProcs) > 0
		case "-":
			return len(r.bgProcs) - 2, len(r.bgProcs) > 1
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n < 1 || n > len(r.bgProcs) {
			return 0, false
		}
		return n - 1, true
	}
	return r.bgIndex(spec)
}

// inheritedJobs copies a shell's job table for a command substitution to
// report on. The channels are shared deliberately -- the copy has to see the
// same jobs finish -- while the entries are marked so `wait` will not treat
// them as this shell's own.
func inheritedJobs(jobs []bgProc) []bgProc {
	if len(jobs) == 0 {
		return nil
	}
	out := make([]bgProc, len(jobs))
	for i, job := range jobs {
		job.inherited = true
		out[i] = job
	}
	return out
}

// bgPID spells a job's identifier the way $! and NAME_PID do, so that the
// output of `jobs -p` can be fed back to `wait` and mean the same job.
func bgPID(i int) string { return "g" + strconv.Itoa(i+1) }

// finished reports whether the job is over, without blocking on it.
func (b *bgProc) finished() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

// jobControlBuiltin implements `fg` and `bg` for a script (#397).
//
// Both are gated on job control, which is off in a script until it writes
// `set -m` — bash answers "no job control" otherwise, and koi answered
// "unsupported builtin", which is a usage error where bash has an
// ordinary status a script can act on.
//
// With job control on, what each can honestly do here follows from koi's
// jobs being goroutines rather than process groups. `fg` waits for the
// job and answers with its status, which is what foregrounding *is* to a
// script — there is no terminal to hand over in a script, and bash's own
// output for it is the job's command line, which is reproduced. `bg` has
// nothing to resume: a goroutine is never stopped, so every job it can
// name is already running, and that is the case bash reports with a
// status of 0.
func (r *Runner) jobControlBuiltin(ctx context.Context, name string, args []string) exitStatus {
	var exit exitStatus
	failf := func(code uint8, format string, a ...any) exitStatus {
		r.errf(format, a...)
		exit.code = code
		return exit
	}
	if !r.opts[optMonitor] {
		return failf(1, "%s: no job control\n", name)
	}
	spec := ""
	if len(args) > 0 {
		spec = args[0]
	}
	if _, err := strconv.Atoi(spec); err == nil {
		// bash takes a bare number, warns, and carries on; the warning
		// is the whole point, since `fg 1` reads as a pid.
		r.errf("%s: warning: %s: job specification requires leading `%%'\n", name, spec)
		spec = "%" + spec
	}
	var (
		i  int
		ok bool
	)
	if spec == "" {
		if i, ok = r.bgCurrent(0); !ok {
			return failf(1, "%s: current: no such job\n", name)
		}
	} else if i, ok = r.bgIndex(spec); !ok {
		return failf(1, "%s: %s: no such job\n", name, spec)
	}
	if r.bgProcs[i].reaped {
		// A job already waited for is gone as far as bash is concerned,
		// whatever its number used to be.
		return failf(1, "%s: %s: no such job\n", name, jobSpecOf(spec, i))
	}
	select {
	case <-r.bgProcs[i].done:
		// A job that has finished has three states in bash and they
		// answer differently, which is only visible with the timing
		// pinned: still running is foregrounded below; finished but
		// never listed is "job has terminated"; and finished *and*
		// already listed is gone, because reporting a job's completion
		// is what drops it from the table.
		if r.bgProcs[i].reported {
			return failf(1, "%s: %s: no such job\n", name, jobSpecOf(spec, i))
		}
		return failf(1, "%s: job has terminated\n", name)
	default:
	}
	if name == "bg" {
		return failf(0, "bg: job %d already in background\n", i+1)
	}
	// bash prints the job's command line as it brings it forward, which
	// is how a script's log says which job it is now waiting on.
	r.outf("%s\n", r.bgProcs[i].cmd)
	select {
	case <-r.bgProcs[i].done:
	case <-ctx.Done():
		exit.fatal(ctx.Err())
		return exit
	}
	r.bgProcs[i].reaped = true
	return *r.bgProcs[i].exit
}

// jobSpecOf names a job the way the message about it should: as the
// caller spelled it, or as "current" when they did not spell it at all.
func jobSpecOf(spec string, i int) string {
	if spec == "" {
		return "current"
	}
	return spec
}

// SignalJob delivers a signal to a background job named by a jobspec —
// `kill %1`, `kill -TERM %%` — and is how the shell around the
// interpreter answers for jobs it does not own (#397).
//
// koi's background jobs are goroutines rather than process groups, so
// what a signal can honestly do here is end one: the job's context is
// cancelled, which is the same mechanism that interrupts a running
// command, and which reaches a real child through the exec handler.
// The job then reports 128+n, the status a signaled process has.
//
// Signal 0 asks whether the job exists and changes nothing, as it does
// for a process. A signal that would *stop* or *continue* a job is
// refused rather than ignored: there is no stopped state for a
// goroutine, and a `kill -STOP` that quietly did nothing would be worse
// than one that says so.
func (r *Runner) SignalJob(spec string, sig syscall.Signal) error {
	i, ok := r.bgIndex(spec)
	if !ok {
		return fmt.Errorf("%s: no such job", spec)
	}
	select {
	case <-r.bgProcs[i].done:
		// A job that has finished is still a job bash will name — and
		// still one it answers 0 for, whether or not a wait has already
		// collected it. There is simply nothing left to signal.
		return nil
	default:
	}
	if sig == 0 {
		return nil
	}
	if verb, ok := jobStopSignal(sig); ok {
		return fmt.Errorf("%s: cannot %s a job: koi runs background jobs in this process", spec, verb)
	}
	if bg := &r.bgProcs[i]; bg.cancel != nil {
		bg.killed.Store(int32(sig))
		bg.cancel()
	}
	return nil
}
