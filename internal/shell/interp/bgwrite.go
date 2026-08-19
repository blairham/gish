// Copyright (c) 2026, Blair Hamilton. See LICENSE for licensing information.

package interp

import (
	"io"
	"os"
	"sync"
)

// syncWriter serializes writes to a writer shared between the shell and its
// background jobs.
//
// bash needs nothing like this: a background job there is a separate process
// holding its own descriptor, so the kernel makes each write(2) indivisible.
// koi's background jobs are goroutines sharing one io.Writer, so two of them
// writing at once is a data race on whatever that writer is -- and for a
// bytes.Buffer the losing write is not interleaved but *dropped*, which is how
// eight lines become six with no error and exit status 0 (#301).
//
// Wrapping is deliberately not applied when the writer is an *os.File: there
// the descriptor is already the serialization point, a short write is atomic
// the same way bash's is, and wrapping would cost more than it buys -- os/exec
// can hand a real file straight to a child process, but anything else forces
// it to interpose a copying goroutine, which would put every external
// command's output through an extra copy and hide the terminal from children
// that ask whether they have one.
type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// shareOutput prepares this runner's stdout and stderr to be written by a
// background job concurrently with the shell itself. It is idempotent, and
// leaves *os.File writers alone for the reasons given on syncWriter.
//
// stdout and stderr share one mutex rather than getting one each, because they
// are frequently the same underlying writer -- `koi -c '...' >file 2>&1', and
// every test that passes one buffer as both -- and two mutexes over one
// destination would serialize nothing.
func (r *Runner) shareOutput() {
	if r.bgWriteMu == nil {
		r.bgWriteMu = new(sync.Mutex)
	}
	r.stdout = syncWrap(r.stdout, r.bgWriteMu)
	r.stderr = syncWrap(r.stderr, r.bgWriteMu)
}

func syncWrap(w io.Writer, mu *sync.Mutex) io.Writer {
	switch w.(type) {
	case nil, *os.File, *syncWriter:
		return w
	}
	return &syncWriter{mu: mu, w: w}
}
