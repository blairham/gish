//go:build !unix

package jobs

import (
	"context"
	"fmt"
	"os"

	"mvdan.cc/sh/v3/interp"
)

// Supported reports whether job control is available on this platform.
// Windows job objects are milestone 7.
func Supported() bool { return false }

// Table is inert off unix: spawning falls through to the next handler
// and the builtins report the limitation.
type Table struct{}

func NewTable(_ *os.File) *Table { return &Table{} }

func (t *Table) BeginLine(string)        {}
func (t *Table) EndLine() (Notice, bool) { return Notice{}, false }

func (t *Table) ExecMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return next
}

func unsupported(hc interp.HandlerContext, name string) error {
	fmt.Fprintf(hc.Stderr, "%s: job control is not supported on this platform\n", name)
	return interp.ExitStatus(1)
}

func (t *Table) Jobs(_ context.Context, hc interp.HandlerContext, _ []string) error {
	return unsupported(hc, "jobs")
}

func (t *Table) Fg(_ context.Context, hc interp.HandlerContext, _ []string) error {
	return unsupported(hc, "fg")
}

func (t *Table) Bg(_ context.Context, hc interp.HandlerContext, _ []string) error {
	return unsupported(hc, "bg")
}

// Count reports filed jobs; always zero without job control.
func (t *Table) Count() int { return 0 }

// Commands is the no-job-control stub; see table_unix.go.
func (t *Table) Commands() []string { return nil }
