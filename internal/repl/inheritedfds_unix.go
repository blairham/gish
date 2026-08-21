//go:build unix

package repl

import (
	"os"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// The descriptors koi was started with (#419).
//
// bash needs no such thing: its redirections are real dup2 calls on real
// descriptors, so a descriptor the process inherited is simply there.
// koi's interpreter models the table instead, which is what lets a
// builtin write to fd 5 without a syscall — and it means an inherited
// descriptor is invisible until something says it exists. `koi 3<&0
// script` and, inside it, `exec <&3`, answered "3: Bad file descriptor".
//
// **The obvious version of this was written once and reverted**, because
// it made a child koi believe fd 4 was open and let `>&$a` with `a=4`
// *succeed* where bash's own suite requires the opposite — a redirection
// that wrongly succeeds being worse than one that wrongly fails. The
// cause was descriptors being renumbered on their way to a child, which
// is fixed: koi lays its table out so entry i is descriptor 3+i and a
// gap is a nil entry, so a child gets fd 6 as fd 6 and gets 3, 4 and 5
// closed. That is what makes reading the numbers meaningful at all.
//
// The second half of not repeating it is FD_CLOEXEC. A Go program opens
// descriptors of its own — the netpoll queue, every file it reads — and
// the runtime marks all of them close-on-exec, which is exactly the
// property being asked about: a descriptor that will not survive an exec
// was never inherited from a parent shell and must not be published to
// one. Filtering on it means this cannot depend on *when* the scan runs.

// inheritedFDs are the descriptors above 2 which this process was
// started with, resolved once per session.
var inheritedFDs = sync.OnceValue(scanInheritedFDs)

// scanInheritedFDs finds the open, inheritable descriptors above 2.
func scanInheritedFDs() map[int]*os.File {
	var found map[int]*os.File
	for _, fd := range openFDs() {
		if fd <= 2 {
			continue // the shell's own standard streams, already modeled
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			continue // closed between listing and asking
		}
		if flags&unix.FD_CLOEXEC != 0 {
			continue // ours, not the caller's
		}
		if found == nil {
			found = make(map[int]*os.File)
		}
		// The name is for diagnostics only; nothing reopens by it.
		found[fd] = os.NewFile(uintptr(fd), "/dev/fd/"+strconv.Itoa(fd))
	}
	return found
}

// fdDirs are the per-platform directories which list a process's own open
// descriptors: one readdir instead of a syscall per number, which matters
// because this is on the startup path (#37).
var fdDirs = []string{"/proc/self/fd", "/dev/fd"}

// openFDs lists the descriptor numbers this process holds.
//
// Reading the directory opens a descriptor of its own, which then shows
// up in its own listing — harmless, because that descriptor is
// close-on-exec and the caller filters those out, and because it is
// closed before anything acts on the list.
func openFDs() []int {
	for _, dir := range fdDirs {
		f, err := os.Open(dir)
		if err != nil {
			continue
		}
		names, err := f.Readdirnames(-1)
		f.Close()
		if err != nil {
			continue
		}
		fds := make([]int, 0, len(names))
		for _, name := range names {
			if fd, err := strconv.Atoi(name); err == nil {
				fds = append(fds, fd)
			}
		}
		return fds
	}
	// No listing available: ask about a bounded range instead. The bound
	// is a compromise rather than a rule — a shell script that passes a
	// descriptor picks a small number, and asking about every descriptor
	// up to the process limit would be a million syscalls before the
	// first prompt.
	fds := make([]int, 0, 32)
	for fd := 3; fd < 256; fd++ {
		fds = append(fds, fd)
	}
	return fds
}
