//go:build !unix && !windows

package repl

// watchResize is a no-op on the platforms with neither SIGWINCH nor a
// console to poll (#110 sequences native Windows after v1; Windows
// itself polls — see winch_windows.go).
func watchResize(func()) (stop func()) { return func() {} }
