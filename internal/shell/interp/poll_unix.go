//go:build unix

package interp

import (
	"context"
	"io"
	"os"
	"time"

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
// waitReadable blocks until f is readable, the deadline passes, or ctx is
// done. A zero deadline means no timeout.
//
// This is the fallback for files the runtime cannot poll — a FIFO opened
// read-write is the common case — where SetReadDeadline is refused and a
// blocked Read would hold the goroutine until the process exits (#348).
// It polls in short slices so a cancelled context still interrupts, since
// the deadline trick the pollable path uses is unavailable here too.
func waitReadable(ctx context.Context, f *os.File, deadline time.Time) (timedOut bool, _ error) {
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		slice := 100 * time.Millisecond
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return true, nil
			}
			slice = min(slice, remaining)
		}
		fds[0].Revents = 0
		// A zero remaining rounds to a zero-millisecond poll, which is
		// the instant answer readyToRead uses; the deadline check above
		// already decided the timeout, so that is fine.
		n, err := unix.Poll(fds, int(slice.Milliseconds())+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		// Any event counts as readable, not just POLLIN: POLLHUP means the
		// next read answers EOF at once, POLLERR an error, and darwin's
		// poll reports /dev/null as POLLNVAL — where a read also answers
		// immediately. Waiting on any of them would block forever on input
		// that is already decided.
		if n > 0 && fds[0].Revents != 0 {
			return false, nil
		}
	}
}

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
