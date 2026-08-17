package builtins

import (
	"context"
	"fmt"

	"mvdan.cc/sh/v3/interp"
)

// newgrp is deliberately not implemented (#61).
//
// It starts a new shell with a different real group id — a privilege
// change plus a re-exec, closer to su than to a shell builtin, and the
// one name in this set where a half-implementation would be a security
// problem rather than an inconvenience. The system's own newgrp does the
// job and is one path away.
//
// It is still claimed here rather than left to the interpreter, for the
// same reason kill and umask were: a name interp recognizes never
// reaches the exec seam, so leaving it alone means shadowing
// /usr/bin/newgrp with "unsupported builtin". Saying what gish does not
// do, and where the real one is, beats a message that reads like a bug.
func Newgrp(_ context.Context, hc interp.HandlerContext, _ []string) error {
	fmt.Fprintln(hc.Stderr,
		"newgrp: not provided by gish — it changes the process group id and re-execs the shell; run /usr/bin/newgrp directly")
	return interp.ExitStatus(1)
}
