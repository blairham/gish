//go:build !unix

package repl

import "os"

// Windows has no descriptor-inheritance model of this shape: handles are
// passed by explicit duplication rather than by number, so there is
// nothing to scan for. See inheritedfds_unix.go for what this answers.
func inheritedFDs() map[int]*os.File { return nil }
