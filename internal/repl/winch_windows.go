//go:build windows

package repl

import (
	"os"
	"time"

	"github.com/blairham/koi-shell/internal/term"
)

// watchResize on Windows: there is no SIGWINCH, so the size is polled
// (#87).
//
// Polling is the right shape here rather than a compromise. The console
// reports a resize as an input *event*, which means the only process
// that can see it is the one currently reading input — and this watcher
// exists precisely for the window when the editor is not reading,
// because a command is running. Reading the console's input queue from
// here would steal keystrokes from the child, which is a far worse bug
// than a resize noticed a quarter of a second late.
//
// A stat-cheap poll of the screen buffer info costs one syscall every
// 250ms, only while the shell is alive, and stops the moment the
// session does.
const resizePollInterval = 250 * time.Millisecond

func watchResize(fn func()) (stop func()) {
	tty := term.NewTTY(os.Stdin, os.Stdout)
	lastW, lastH, err := tty.Size()
	if err != nil {
		// No console (a piped session, a service): nothing will ever
		// resize, so the watcher is a no-op rather than a busy loop.
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(resizePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w, h, err := tty.Size()
				if err != nil || (w == lastW && h == lastH) {
					continue
				}
				lastW, lastH = w, h
				fn()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}
