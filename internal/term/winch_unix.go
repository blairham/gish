//go:build unix

package term

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyResize delivers a tick per SIGWINCH; the caller queries Size.
func notifyResize() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch, func() { signal.Stop(ch) }
}
