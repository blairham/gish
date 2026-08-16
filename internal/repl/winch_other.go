//go:build !unix

package repl

// watchResize is a no-op where SIGWINCH does not exist; capture is
// unix-only for now (#110 sequences native Windows after v1).
func watchResize(func()) (stop func()) { return func() {} }
