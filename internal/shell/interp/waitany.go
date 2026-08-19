package interp

import (
	"strconv"
	"strings"
)

// waitNoChildren is what `wait -n` answers when there is no unreaped job
// left to wait for. bash uses 127 — the same status as a command that does
// not exist, which is the closest thing it has to "there is nothing there".
//
// It is the loop's terminating condition rather than an error: a bounded
// parallel loop drains its jobs with `wait -n` until this comes back.
const waitNoChildren = 127

// bgIndex resolves a job spelling — "g" plus the 1-indexed position, the
// same form $! hands out — to an index into bgProcs.
func (r *Runner) bgIndex(arg string) (int, bool) {
	rest, ok := strings.CutPrefix(arg, "g")
	pid := atoi(rest)
	if !ok || pid <= 0 || pid > int64(len(r.bgProcs)) {
		return 0, false
	}
	return int(pid) - 1, true
}

// waitAny implements `wait -n`: block until the *next* job finishes and
// answer with its status (#287).
//
// Without it there is no way to block until *some* job finishes, so the
// bounded parallel loop every build script and test runner reaches for
//
//	for item in "${items[@]}"; do
//	  while (( $(jobs -r | wc -l) >= MAX )); do wait -n; done
//	  process "$item" &
//	done
//
// could not be written: plain `wait` blocks for all of them, which turns
// the loop into batches and defeats the point.
//
// pids restricts the candidates the way `wait -n PID…` does; empty means
// every job the shell still has. pidVar is `-p`'s variable, set to the job
// that answered and left unset when none did.
func (r *Runner) waitAny(pids []string, pidVar string) exitStatus {
	var candidates []int
	if len(pids) == 0 {
		for i := range r.bgProcs {
			if !r.bgProcs[i].reaped {
				candidates = append(candidates, i)
			}
		}
	} else {
		for _, arg := range pids {
			i, ok := r.bgIndex(arg)
			if !ok {
				r.errf("wait: pid %s is not a child of this shell\n", arg)
				return exitStatus{code: 1}
			}
			if !r.bgProcs[i].reaped {
				candidates = append(candidates, i)
			}
		}
	}
	if len(candidates) == 0 {
		return exitStatus{code: waitNoChildren}
	}

	// A job that has already finished satisfies `wait -n` immediately —
	// bash hands back a finished-but-unreaped job rather than blocking for
	// the next one to finish. Where several are already finished, this
	// takes the earliest job rather than whichever the runtime happens to
	// pick, so a script's output does not depend on scheduling.
	for _, i := range candidates {
		select {
		case <-r.bgProcs[i].done:
			return r.reap(i, pidVar)
		default:
		}
	}

	// Nothing has finished, so block on all of them at once. The channel
	// is buffered to the number of watchers, so the ones that lose the
	// race still send and exit rather than blocking forever on a receiver
	// that has moved on — there is no goroutine here that outlives the job
	// it is watching.
	finished := make(chan int, len(candidates))
	for _, i := range candidates {
		go func(i int) {
			<-r.bgProcs[i].done
			finished <- i
		}(i)
	}
	return r.reap(<-finished, pidVar)
}

// reap marks a job collected and answers with its status, recording which
// job it was in `-p`'s variable when there is one.
func (r *Runner) reap(i int, pidVar string) exitStatus {
	r.bgProcs[i].reaped = true
	if pidVar != "" {
		r.setVarString(pidVar, "g"+strconv.Itoa(i+1))
	}
	return *r.bgProcs[i].exit
}
