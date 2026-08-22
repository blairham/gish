//go:build !(darwin || dragonfly || freebsd || netbsd || openbsd)

package interp

import "os"

// fdAccess declines to answer where a `/dev/fd` entry reopens the file it
// points at rather than duplicating the descriptor — Linux, and anything
// with no /dev/fd at all. bash there opens the file afresh in whichever
// mode was asked for, so a refusal keyed on the descriptor's own mode would
// be koi refusing what the local bash allows (#645, and #667 for the gap
// this leaves: koi hands out the descriptor either way, so the other
// direction fails on the first read or write instead of succeeding).
func fdAccess(f *os.File) (readable, writable, known bool) {
	return false, false, false
}
