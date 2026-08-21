//go:build unix

package builtins

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// times (#55).
//
// The last of the interpreter-claimed names that could be implemented
// cheaply. It is not a common builtin, but it was in the same broken
// state as kill and umask — recognized, unimplemented, and therefore
// shadowing the real one rather than falling through to it — and the
// cost of finishing it is a syscall each for self and children.
//
// The output is POSIX's: two lines, shell then children, user time then
// system time, in bash's `0m0.000s` shape.
func Times(_ context.Context, hc interp.HandlerContext, _ []string) error {
	var self, children syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &self); err != nil {
		hc.Errf("times: %v\n", err)
		return interp.ExitStatus(1)
	}
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &children); err != nil {
		hc.Errf("times: %v\n", err)
		return interp.ExitStatus(1)
	}
	fmt.Fprintf(hc.Stdout, "%s %s\n", clockTime(self.Utime), clockTime(self.Stime))
	fmt.Fprintf(hc.Stdout, "%s %s\n", clockTime(children.Utime), clockTime(children.Stime))
	return nil
}

// clockTime renders a timeval the way times(1) does: whole minutes, then
// seconds to three places.
func clockTime(tv syscall.Timeval) string {
	d := time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
	return fmt.Sprintf("%dm%.3fs", int(d.Minutes()), d.Seconds()-float64(int(d.Minutes()))*60)
}
