//go:build !darwin && !linux

package sandbox

import "fmt"

// enforceAndExec: no enforcement backend on this platform yet —
// Windows lands with the #47 milestone. Refusing beats pretending.
func enforceAndExec(Policy, []string, []string) error {
	return fmt.Errorf("sandbox: not supported on this platform yet")
}

// Available reports the enforcement backend for status displays.
func Available() string { return "not supported on this platform (#47)" }
