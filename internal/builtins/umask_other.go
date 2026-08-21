//go:build !unix

package builtins

import (
	"context"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// Umask is unavailable where there is no umask syscall. Registered
// anyway, so the name resolves and says what is missing rather than
// reporting "executable file not found".
func Umask(_ context.Context, hc interp.HandlerContext, _ []string) error {
	hc.Errf("umask: not supported on this platform\n")
	return interp.ExitStatus(1)
}
