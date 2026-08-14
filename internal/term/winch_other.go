//go:build !unix

package term

import "os"

// notifyResize is a no-op off unix; ConPTY resize arrives as a
// WindowSizeEvent through the input decoder instead (milestone 7).
func notifyResize() (<-chan os.Signal, func()) {
	return nil, func() {}
}
