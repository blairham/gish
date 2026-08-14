//go:build !unix

package repl

// ignoreTTOU is a no-op off unix; terminal process groups don't exist
// there (job control is milestone 7).
func ignoreTTOU() {}
