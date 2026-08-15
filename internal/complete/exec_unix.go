//go:build !windows

package complete

import "io/fs"

// executableName reports whether a PATH entry is a command and the
// name it completes as: on unix, the executable bit decides and the
// name is used as-is.
func executableName(e fs.DirEntry) (string, bool) {
	info, err := e.Info()
	if err != nil || info.Mode()&0o111 == 0 {
		return "", false
	}
	return e.Name(), true
}
