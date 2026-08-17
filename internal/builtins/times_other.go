//go:build !unix

package builtins

import (
	"context"
	"fmt"

	"mvdan.cc/sh/v3/interp"
)

// Times is unavailable where getrusage is. Registered anyway, so the
// name resolves and says what is missing.
func Times(_ context.Context, hc interp.HandlerContext, _ []string) error {
	fmt.Fprintln(hc.Stderr, "times: not supported on this platform")
	return interp.ExitStatus(1)
}
