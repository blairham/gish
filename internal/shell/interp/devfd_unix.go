//go:build unix

package interp

import (
	"os"

	"golang.org/x/sys/unix"
)

// fdAccess reports whether the descriptor behind f can be read and written,
// and whether the question could be answered at all.
//
// Opening `/dev/fd/N` for writing when N was opened for reading is
// "Permission denied" in bash rather than an open which then writes
// nothing (#645), and the shell's descriptor table does not record the
// mode a file was opened with — the kernel does, and this is how it is
// asked.
func fdAccess(f *os.File) (readable, writable, known bool) {
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
	if err != nil {
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
