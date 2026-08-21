//go:build !unix

package interp

import "syscall"

// jobStopSignal has nothing to answer where the stop signals are not
// defined; see the unix file for what it is for.
func jobStopSignal(syscall.Signal) (string, bool) { return "", false }
