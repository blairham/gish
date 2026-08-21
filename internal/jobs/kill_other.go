//go:build !unix

package jobs

import (
	"context"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// Kill is unavailable where job control is (#47 tracks the Windows
// port). It is still registered, so the name resolves and says what is
// missing rather than reporting "executable file not found" — the same
// rule the native builtin matrix pins for every other command.
func (t *Table) Kill(_ context.Context, hc interp.HandlerContext, _ []string) error {
	hc.Errf("kill: not supported on this platform\n")
	return interp.ExitStatus(1)
}
