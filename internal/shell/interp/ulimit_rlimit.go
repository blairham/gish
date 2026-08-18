//go:build darwin || linux

package interp

import "golang.org/x/sys/unix"

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
	return lim.Cur, nil
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
	return unix.Setrlimit(resource, &lim)
}
