//go:build windows

package complete

import (
	"io/fs"
	"os"
)

// executableName reports whether a PATH entry is a command and the
// name it completes as: on Windows, PATHEXT decides, and the extension
// is stripped — the user types `git`, not `git.exe` (exec's LookPath
// resolves the extension, so the completed name runs).
func executableName(e fs.DirEntry) (string, bool) {
	pathext := os.Getenv("PATHEXT")
	if pathext == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	return stripExecutableExt(e.Name(), pathext)
}
