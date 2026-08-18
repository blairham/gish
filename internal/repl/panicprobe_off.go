//go:build !koipanicprobe

package repl

import "mvdan.cc/sh/v3/interp"

// panicProbeCallHandler is a passthrough in every build that is not the
// panic-guard test build. See panicprobe.go for why the probe exists.
func panicProbeCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return next
}
