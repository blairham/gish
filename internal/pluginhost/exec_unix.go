//go:build !windows

package pluginhost

import "io/fs"

// executableCandidate reports whether a discovered file can be a
// plugin: on unix, the executable bit.
func executableCandidate(fi fs.FileInfo) bool {
	return fi.Mode()&0o111 != 0
}
