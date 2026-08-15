//go:build windows

package pluginhost

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// executableCandidate reports whether a discovered file can be a
// plugin: Windows has no exec bit, so the executable extensions decide
// (#47).
func executableCandidate(fi fs.FileInfo) bool {
	switch strings.ToLower(filepath.Ext(fi.Name())) {
	case ".exe", ".bat", ".cmd", ".com":
		return true
	}
	return false
}
