//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain removes the shared test binary. Tagged unix because
// buildKoi and its callers are — the pty tests that need a built
// binary do not run elsewhere.
//
// buildKoi deliberately does not use t.TempDir — the binary outlives any one test — so cleanup
// belongs here rather than being left to the OS.
func TestMain(m *testing.M) {
	code := m.Run()
	if bin, err := buildOnce(); err == nil {
		_ = os.RemoveAll(filepath.Dir(bin)) //nolint:errcheck // teardown
	}
	os.Exit(code)
}
