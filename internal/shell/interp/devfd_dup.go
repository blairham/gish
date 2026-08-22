//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package interp

import (
	"os"

	"golang.org/x/sys/unix"
)

// fdAccess reports whether the descriptor behind f can be read and written,
// and whether the question could be answered at all (#645).
//
// It is asked only where a `/dev/fd` entry is a **dup** of the descriptor,
// which is the BSD family: there the mode the descriptor was opened with is
// binding, so `. /dev/fd/N` on a write-only one is bash's "Permission
// denied" rather than an open which then reads nothing. Linux's
// /proc/self/fd reopens the *file* instead, where bash allows it and this
// question is not the one being asked — hence the platform split rather
// than a `unix` build tag.
//
// The descriptor is read through SyscallConn rather than [os.File.Fd],
// which would take the file out of the runtime's poller and make it
// blocking — the shell's standard input is often a pipe whose read
// deadline is what lets a blocked read be interrupted.
func fdAccess(f *os.File) (readable, writable, known bool) {
	rc, err := f.SyscallConn()
	if err != nil {
		return false, false, false
	}
	var flags int
	var ferr error
	if cerr := rc.Control(func(fd uintptr) {
		flags, ferr = unix.FcntlInt(fd, unix.F_GETFL, 0)
	}); cerr != nil || ferr != nil {
		return false, false, false
	}
	switch flags & unix.O_ACCMODE {
	case unix.O_RDONLY:
		return true, false, true
	case unix.O_WRONLY:
		return false, true, true
	case unix.O_RDWR:
		return true, true, true
	}
	return false, false, false
}
