//go:build !unix

package builtins

import (
	"context"
	"fmt"

	"mvdan.cc/sh/v3/interp"
)

// Umask is unavailable where there is no umask syscall. Registered
// anyway, so the name resolves and says what is missing rather than
// reporting "executable file not found".
func Umask(_ context.Context, hc interp.HandlerContext, _ []string) error {
	fmt.Fprintln(hc.Stderr, "umask: not supported on this platform")
	return interp.ExitStatus(1)
}
