//go:build darwin || linux

package interp

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// rlimInfinity is the kernel's "no limit", which bash spells "unlimited"
// in both directions.
//
// It is not the same number everywhere — darwin's is 0x7fff…, linux's is
// 0xffff… — which is why "is this unlimited" is asked through
// isUnlimited rather than by comparing against the largest uint64.
const rlimInfinity = uint64(unix.RLIM_INFINITY)

func isUnlimited(raw uint64) bool { return raw == rlimInfinity }

func getRlimit(resource int, hard bool) (uint64, error) {
	var lim unix.Rlimit
	if err := unix.Getrlimit(resource, &lim); err != nil {
		return 0, err
	}
	if hard {
		return lim.Max, nil
	}
	if resource == unix.RLIMIT_NOFILE {
		if cur, ok := childNofile(); ok {
			return cur, nil
		}
	}
	return lim.Cur, nil
}

// nofileShellSet records that this shell set RLIMIT_NOFILE itself, after
// which its own limit is the honest answer again — see childNofile.
var nofileShellSet atomic.Bool

// probedNofile asks once, lazily, and only if anything asks at all.
var probedNofile = sync.OnceValues(probeChildNofile)

// childNofile reports the soft open-file limit this shell's children
// receive, when that is not the limit the shell itself is running under.
//
// The Go runtime raises RLIMIT_NOFILE for its own process before main
// starts — deliberately, after go.dev/issue/46279, because Go does not use
// select and should not inherit a limit sized for fd_set. It then restores
// the original for every process it starts, so a Go program's own limit
// and its children's are two different numbers. For most programs that is
// invisible. For a shell it is the answer to a question users ask: `ulimit
// -n` reported 245760 here while `bash` reported 1048576 and every command
// koi ran got 1048576 (#294). The shell was describing itself where it was
// being asked about the programs it runs.
//
// The original is not recoverable in-process: the runtime keeps it in an
// unexported variable, reachable neither by an API nor, since Go 1.23, by
// a linkname. Nor can the probe be a Go program — a Go child raises its
// own limit at init exactly like the parent did, and would report the
// raised number rather than the inherited one. So the question goes to a
// child that is not Go, which is what makes the answer the true one: it is
// measured from the child side rather than reasoned about from this one.
//
// Once the shell sets the limit itself, this stops applying. setRlimit
// goes through syscall.Setrlimit, which tells the runtime to stop
// substituting anything into children, so from then on the shell's limit
// really is what children get.
func childNofile() (uint64, bool) {
	if nofileShellSet.Load() {
		return 0, false
	}
	return probedNofile()
}

// probeChildNofile runs the one question a shell can always ask another
// shell. A failure — no /bin/sh, a refusal, an answer that is not a
// number — falls back to reporting this process's own limit, which is what
// koi did before and is wrong only in the way this exists to describe.
func probeChildNofile() (uint64, bool) {
	out, err := exec.Command("/bin/sh", "-c", "ulimit -n").Output()
	if err != nil {
		return 0, false
	}
	answer := strings.TrimSpace(string(out))
	if answer == "unlimited" {
		return rlimInfinity, true
	}
	n, err := strconv.ParseUint(answer, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// setRlimit changes the named halves of a limit and leaves the others as
// they were. Both halves go to the kernel together, so the current pair
// is read first: writing a zero into the half not being set would drop
// the hard limit to nothing, and a process can never raise one back.
//
// unix.Setrlimit rather than a raw syscall on purpose. It forwards to
// syscall.Setrlimit, which tells the runtime to stop substituting its
// own RLIMIT_NOFILE into child processes — without that, `ulimit -n 512`
// would move the shell's limit and every program it then ran would
// silently get the old one.
func setRlimit(resource int, soft, hard bool, raw uint64) error {
	var lim unix.Rlimit
	if err := unix.Getrlimit(resource, &lim); err != nil {
		return err
	}
	if soft {
		lim.Cur = raw
	}
	if hard {
		lim.Max = raw
	}
	if resource == unix.RLIMIT_NOFILE {
		// From here the runtime substitutes nothing, so this shell's own
		// limit is what children get and reporting it is honest again.
		nofileShellSet.Store(true)
	}
	return unix.Setrlimit(resource, &lim)
}
