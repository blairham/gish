//go:build unix

package repl

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize calls fn on every terminal size change until the returned
// stop is called.
//
// This is a second SIGWINCH subscriber alongside the line editor's, and
// that is fine: signal.Notify fans out to every registered channel. The
// editor only observes resizes while it is reading a line, and a
// command running under output capture (#99) needs its pty resized
// exactly when the editor is *not* reading.
func watchResize(fn func()) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				fn()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
