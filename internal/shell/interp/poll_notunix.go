//go:build !unix

package interp

import (
	"context"
	"io"
	"os"
	"time"
)

// readTimeoutStatus is what `read -t` exits with when the timeout fires;
// see the unix file for why this number.
const readTimeoutStatus = 128 + 14

// readyToRead answers `read -t 0` off unix, where there is no poll to ask.
//
// It reports ready rather than not-ready, because the two wrong answers
// are not equally wrong: "ready" makes the following read behave normally,
// while "not ready" would make a polling loop report no input forever on a
// stream that has plenty. Reading a byte to find out is not an option —
// that would consume the input the caller was only asking about.
func readyToRead(io.Reader) (bool, error) { return true, nil }

// waitReadable has no poll to ask off unix, so it reports readable at
// once and the following read blocks as it always has there.
func waitReadable(context.Context, *os.File, time.Time) (bool, error) { return false, nil }
