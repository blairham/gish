// Package capture records a command's output while it still runs on a
// terminal (#99 stage 2).
//
// The constraint that decides the design: a shell that wants to see its
// children's output has to sit in the middle of it, and the obvious way
// — a pipe — breaks the world. `isatty` goes false, so `git log` stops
// paging, `ls` stops coloring, and `docker` stops drawing progress
// bars. A capture feature that silently changes program behavior is
// worse than no capture feature.
//
// So: a PTY. The child still sees a terminal and behaves identically.
//
// # Why this does not have to reimplement job control
//
// docs/blocks.md anticipated that koi would have to take over what the
// terminal owned — relaying signals, forwarding window-size changes,
// interleaving a copy loop with a race-sensitive foreground handoff.
// That is true of the shape where the PTY becomes the child's
// controlling terminal (what script(1) does, and what a terminal
// emulator does).
//
// It is not the shape used here. The child gets the PTY for **stdout
// only**; its stdin, its stderr, and its *controlling terminal* remain
// the real ones, exactly as before. Verified: a child in this
// arrangement reports `isatty(stdout)=yes`, reads the PTY's window size
// correctly, and still names the real tty as its controlling terminal.
//
// stderr staying real is load-bearing, not incidental — see the note in
// internal/jobs where the substitution happens. Full-screen programs do
// their terminal control on fd 2.
//
// That single difference is load-bearing. Ctrl-C, Ctrl-Z, SIGWINCH and
// the foreground-group handoff all keep working *because nothing about
// them changed* — the kernel still delivers them from the real terminal
// to the child's process group. Job control is preserved by
// construction rather than by careful reimplementation, which is the
// difference between a feature that can ship opt-in and one that cannot.
//
// The cost, stated honestly: a program that writes directly to /dev/tty
// bypasses capture (correct — that is what /dev/tty is for), and output
// crosses one extra copy on its way to the screen.
package capture

import (
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/creack/pty"
)

// DefaultLimit caps one command's retained output. Enough for a build
// log's tail, small enough that a runaway `yes` costs bounded memory.
const DefaultLimit = 256 << 10

// Session is one command line's capture. It owns a PTY whose slave the
// child writes to, and copies everything through to the real terminal
// while keeping the tail.
type Session struct {
	master *os.File
	slave  *os.File
	out    io.Writer

	buf  *ring
	done chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// Start opens a capture session that mirrors to out, which is the real
// terminal. cols and rows size the PTY; a program asking its terminal
// how wide it is must get the real answer or its output wraps wrongly.
func Start(out io.Writer, cols, rows int, limit int) (*Session, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	s := &Session{
		master: master,
		slave:  slave,
		out:    out,
		buf:    newRing(limit),
		done:   make(chan struct{}),
	}
	s.Resize(cols, rows)
	go s.pump()
	return s, nil
}

// Slave is the file the child should use for stdout.
//
// Deliberately not stdin and not stderr. Leaving stdin alone keeps the
// child's controlling terminal — and therefore its job control —
// untouched; leaving stderr alone keeps full-screen programs working,
// because that is the descriptor they configure the terminal through.
func (s *Session) Slave() *os.File { return s.slave }

// Resize matches the PTY to the real terminal. The child learns about
// size changes from its controlling terminal (still the real one), so
// this exists purely so that ioctl(TIOCGWINSZ) on stdout returns the
// truth.
func (s *Session) Resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	_ = pty.Setsize(s.master, &pty.Winsize{ //nolint:errcheck // best-effort; a wrong size is cosmetic
		Cols: uint16(cols), //nolint:gosec // terminal dimensions
		Rows: uint16(rows), //nolint:gosec // terminal dimensions
	})
}

// pump copies the PTY through to the terminal, teeing into the ring.
//
// Writing to the terminal first and recording second is deliberate: if
// the two ever disagree, the user's screen wins. Capture is a history
// feature; it must never be the reason output is late or missing.
func (s *Session) pump() {
	defer close(s.done)
	chunk := make([]byte, 32<<10)
	for {
		n, err := s.master.Read(chunk)
		if n > 0 {
			if s.out != nil {
				_, _ = s.out.Write(chunk[:n]) //nolint:errcheck // the terminal is not ours to fail on
			}
			s.buf.Write(chunk[:n])
		}
		if err != nil {
			return
		}
	}
}

// Close ends the session and returns the retained output.
//
// Ordering matters and is easy to get wrong. The slave must be closed
// first: while any slave descriptor is open the master never reports
// end-of-file, so draining before closing would hang forever. Once it
// is closed the pump drains what the kernel still holds and exits,
// which is what the wait on done is for — closing the master early
// would discard a command's last few lines, and losing the tail of a
// build log is precisely the case this feature exists for.
func (s *Session) Close() []byte {
	s.closeOnce.Do(func() {
		s.closeErr = s.slave.Close()
		select {
		case <-s.done:
			// Everything holding the slave let go; the pump saw EOF.
		case <-time.After(drainGrace):
			// Something still holds a slave descriptor — a background
			// job that inherited it, or a daemon the command left
			// behind — so the master will never report end-of-file and
			// waiting is waiting forever. Closing the master unblocks
			// the pump's read.
			//
			// A capture that hangs the shell is infinitely worse than a
			// capture that loses a few bytes, and this bound is what
			// makes "capture never delays the prompt" true even when a
			// child misbehaves.
		}
		_ = s.master.Close() //nolint:errcheck // teardown
	})
	return s.buf.Bytes()
}

// drainGrace bounds how long Close waits for the pump after the slave
// is closed. Long enough that a normal command's tail always lands,
// short enough that nobody notices when it does not.
const drainGrace = 250 * time.Millisecond

// Bytes returns what has been captured so far without ending the
// session.
func (s *Session) Bytes() []byte { return s.buf.Bytes() }

// ring is a fixed-capacity byte buffer that keeps the most recent
// bytes. The tail is what matters: a failed build's error is at the
// end, and the first 256KB of a `find /` is worth nothing.
type ring struct {
	mu       sync.Mutex
	data     []byte
	limit    int
	overflow bool
}

func newRing(limit int) *ring { return &ring{limit: limit} }

func (r *ring) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, p...)
	if len(r.data) > r.limit {
		drop := len(r.data) - r.limit
		r.data = r.data[drop:]
		r.overflow = true
	}
}

func (r *ring) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.data))
	copy(out, r.data)
	return out
}

// Truncated reports whether output was dropped, so a caller can say so
// rather than presenting a partial log as if it were complete.
func (s *Session) Truncated() bool {
	s.buf.mu.Lock()
	defer s.buf.mu.Unlock()
	return s.buf.overflow
}

// ErrUnsupported means this platform has no PTY support; the caller
// runs the command uncaptured rather than not at all.
var ErrUnsupported = errors.New("capture: pty unavailable on this platform")
