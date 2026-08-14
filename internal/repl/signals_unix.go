//go:build unix

package repl

import (
	"os/signal"
	"syscall"
)

// ignoreTTOU keeps the shell alive when it reclaims the terminal from
// the background (#5). Unix-only: Windows has no SIGTTOU.
func ignoreTTOU() {
	signal.Ignore(syscall.SIGTTOU)
}
