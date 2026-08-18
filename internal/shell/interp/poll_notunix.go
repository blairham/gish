//go:build !unix

package interp

import "io"

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
