//go:build !windows

package p10k

import "golang.org/x/sys/unix"

// writable reports whether the directory can be written to, which the
// dir segment shows as a state. access(2) answers in one syscall against
// a path the kernel has almost certainly just cached — cheap enough for
// the prompt path, and only asked for when DIR_SHOW_WRITABLE is on.
func writable(dir string) bool {
	return unix.Access(dir, unix.W_OK) == nil
}
