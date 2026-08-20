//go:build !unix

package interp

import "os"

// lookupSignal knows no real signals off unix, so every spec that is not
// one of the fake traps is refused the way an unknown name is.
func lookupSignal(string) (string, os.Signal, bool) { return "", nil, false }

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
