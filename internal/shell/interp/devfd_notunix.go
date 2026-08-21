//go:build !unix

package interp

import "os"

// fdAccess cannot answer off unix, where there is no F_GETFL; the caller
// then treats the descriptor as usable in either direction, which is what
// koi did everywhere before it could ask (#645).
func fdAccess(f *os.File) (readable, writable, known bool) {
	return false, false, false
}
