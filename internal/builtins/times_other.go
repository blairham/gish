//go:build !unix

package builtins

import (
	"context"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// Times is unavailable where getrusage is. Registered anyway, so the
// name resolves and says what is missing.
func Times(_ context.Context, hc interp.HandlerContext, _ []string) error {
	hc.Errf("times: not supported on this platform\n")
	return interp.ExitStatus(1)
}
