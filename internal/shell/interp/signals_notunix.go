//go:build !unix

package interp

type signalEntry struct {
	name string
	num  int
}

// signalList is empty off unix, so `trap -l` prints nothing rather than
// a table of numbers that mean nothing here. Windows has no signals to
// trap in the POSIX sense (#87 tracks what interactive support means
// there), and inventing plausible numbers would be worse than silence:
// the numbers exist to be handed to `kill`.
func signalList() []signalEntry { return nil }
