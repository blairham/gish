package tools

import (
	"os"
	"strings"
)

// Resolves reports whether the pin finds an installed version under
// the roots (or is satisfied without one, like `system`).
func (p Pin) Resolves(roots []string) bool {
	_, ok := resolvePin(p, roots)
	return ok
}

// SetPin rewrites tool's line in a .tool-versions file, or appends
// one; comments and unrelated lines are preserved. The file is created
// when missing.
func SetPin(path, tool, version string) error {
	data, err := os.ReadFile(path) //nolint:gosec // the user's own pins file
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(string(data), "\n")
	replaced := false
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == tool {
			lines[i] = tool + " " + version
			replaced = true
		}
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if !replaced {
		lines = append(lines, tool+" "+version)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644) //nolint:gosec // pins are not a secret
}
