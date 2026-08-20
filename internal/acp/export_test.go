package acp

import (
	"testing"
	"time"
)

// SetWaitDelay shrinks the reap backstop for a test, so exercising a
// deliberately pipe-holding descendant does not cost the suite the full
// production delay.
func SetWaitDelay(t *testing.T, d time.Duration) {
	t.Helper()
	old := waitDelay
	waitDelay = d
	t.Cleanup(func() { waitDelay = old })
}
