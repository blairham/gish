//go:build unix

package repl

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// The scan must publish what the process inherited and nothing else
// (#419). Its predecessor was reverted for publishing koi's own
// descriptors to scripts, which made a redirection wrongly *succeed* —
// so the discriminator, FD_CLOEXEC, is what these two cases are about.
func TestScanInheritedFDsSkipsOurOwnDescriptors(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "ours"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ours := int(f.Fd())

	// Go opens every file close-on-exec, which is exactly what "not
	// inherited from a parent, and not surviving to a child" means.
	if flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0); err != nil {
		t.Fatal(err)
	} else if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("fd %d is not close-on-exec, so this test proves nothing", ours)
	}
	if got := scanInheritedFDs(); got[ours] != nil {
		t.Errorf("the scan published koi's own descriptor %d", ours)
	}

	// The same file on an inheritable descriptor is a different answer:
	// dup3 without FD_CLOEXEC is what a caller's `3<file` leaves behind.
	inherited, err := unix.Dup(ours)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(inherited)
	if _, err := unix.FcntlInt(uintptr(inherited), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	found := scanInheritedFDs()
	if found[inherited] == nil {
		t.Errorf("the scan missed inheritable descriptor %d: found %v", inherited, keysOf(found))
	}
	if found[ours] != nil {
		t.Errorf("the scan published koi's own descriptor %d", ours)
	}
	for fd := range found {
		if fd <= 2 {
			t.Errorf("the scan published descriptor %d, which is a standard stream", fd)
		}
	}
}

func keysOf(m map[int]*os.File) []int {
	fds := make([]int, 0, len(m))
	for fd := range m {
		fds = append(fds, fd)
	}
	return fds
}
