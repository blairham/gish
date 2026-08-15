package complete

import (
	"path/filepath"
	"strings"
)

// stripExecutableExt matches name against a PATHEXT-style list
// (";"-separated, case-insensitive) and returns the extension-stripped
// command name. Portable so the logic is testable everywhere; only
// Windows wires it to PATH scanning.
func stripExecutableExt(name, pathext string) (string, bool) {
	ext := filepath.Ext(name)
	if ext == "" {
		return "", false
	}
	for _, allowed := range strings.Split(pathext, ";") {
		if allowed != "" && strings.EqualFold(ext, allowed) {
			return strings.TrimSuffix(name, ext), true
		}
	}
	return "", false
}
