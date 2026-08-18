//go:build unix

package interp

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// readTimeoutStatus is what `read -t` exits with when the timeout fires.
//
// bash reports it the way it reports a signal — 128 + SIGALRM — and the
// only contract a script relies on is "greater than 128", which is how
// `read -t 5 x || echo timed out` tells a timeout from an end of input.
const readTimeoutStatus = 128 + 14 // SIGALRM

// readyToRead answers `read -t 0`: is there input waiting, without taking
// any of it?
//
// It has to be a poll rather than a read with a zero deadline, because a
// read would consume the byte it was asked about — a script polling in a
// loop would eat its own input one character at a time, which is worse
// than the option not existing.
//
// A source that is not a real file cannot be polled and is reported ready:
// it is an in-memory reader supplied by an embedder, where a read will not
// block, and "ready" is the answer that makes the next read behave.
func readyToRead(src io.Reader) (bool, error) {
	f, ok := src.(*os.File)
	if !ok {
		return true, nil
	}
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		// POLLHUP counts as ready: the far end is gone, so the next read
		// returns EOF immediately rather than blocking, and bash likewise
		// reports a closed pipe as something to read.
		return n > 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
	}
}
