// Package tools resolves .tool-versions pins against installed runtime
// versions (#77): asdf-class version switching without shims, hooks, or
// a subprocess on cd. The shell owns the directory-change moment, so
// activation is a PATH computation, not a callback into another tool.
//
// Trust posture: no allow-prompt needed — a repository's .tool-versions
// can only select among versions already installed under the managed
// install tree; every resolved PATH entry points inside it. A pin for
// an uninstalled version resolves to nothing (reported as Missing).
package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Pin is one .tool-versions line: a tool and its candidate versions in
// preference order (asdf allows fallbacks on one line).
type Pin struct {
	Tool     string
	Versions []string
}

// Resolution is what a directory's pins amount to on this machine.
type Resolution struct {
	// File is the .tool-versions that applied ("" = none found).
	File string
	// Bins are the resolved bin directories, pin order, ready to
	// prepend to PATH.
	Bins []string
	// Missing are pins with no installed version to resolve to.
	Missing []Pin
}

// InstallRoot returns the asdf install tree: $ASDF_DATA_DIR/installs,
// defaulting to ~/.asdf/installs. (The mise tree lands with v2.)
func InstallRoot() string {
	if dir := os.Getenv("ASDF_DATA_DIR"); dir != "" {
		return filepath.Join(dir, "installs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".asdf", "installs")
}

// Resolve walks up from dir for the nearest .tool-versions (falling
// back to the home-directory file, asdf's global), parses its pins,
// and resolves each against the install root.
func Resolve(dir, installRoot string) Resolution {
	file := findFile(dir)
	if file == "" {
		return Resolution{}
	}
	res := Resolution{File: file}
	for _, pin := range ParseFile(file) {
		if bins, ok := resolvePin(pin, installRoot); ok {
			res.Bins = append(res.Bins, bins...)
		} else {
			res.Missing = append(res.Missing, pin)
		}
	}
	return res
}

// findFile walks dir upward for .tool-versions; the nearest wins. When
// no directory has one, the home file applies (the asdf global).
func findFile(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		candidate := filepath.Join(d, ".tool-versions")
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	if home, err := os.UserHomeDir(); err == nil && !strings.HasPrefix(dir, home) {
		candidate := filepath.Join(home, ".tool-versions")
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
	}
	return ""
}

// ParseFile reads pins: `tool version [fallback…]`, # comments, blank
// lines skipped. A malformed line is skipped, never fatal.
func ParseFile(path string) []Pin {
	data, err := os.ReadFile(path) //nolint:gosec // the user's own pins file
	if err != nil {
		return nil
	}
	var pins []Pin
	for line := range strings.Lines(string(data)) {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pins = append(pins, Pin{Tool: fields[0], Versions: fields[1:]})
	}
	return pins
}

// resolvePin finds the first installed candidate version and returns
// its bin directories. ok=true with no bins means the pin is satisfied
// without PATH changes ("system" — use whatever PATH already has).
func resolvePin(pin Pin, installRoot string) (bins []string, ok bool) {
	for _, version := range pin.Versions {
		switch {
		case version == "system":
			return nil, true
		case strings.HasPrefix(version, "path:"):
			if dirs := binDirs(strings.TrimPrefix(version, "path:")); len(dirs) > 0 {
				return dirs, true
			}
		default:
			if installRoot == "" {
				continue
			}
			if dirs := binDirs(filepath.Join(installRoot, pin.Tool, version)); len(dirs) > 0 {
				return dirs, true
			}
		}
	}
	return nil, false
}

// binDirs finds the executable directories under one installed version:
// <dir>/bin plus any <dir>/*/bin — the two layouts asdf plugins
// produce (golang famously has both bin/ and go/bin/).
func binDirs(dir string) []string {
	var out []string
	if isDir(filepath.Join(dir, "bin")) {
		out = append(out, filepath.Join(dir, "bin"))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "bin" {
			continue
		}
		if nested := filepath.Join(dir, e.Name(), "bin"); isDir(nested) {
			out = append(out, nested)
		}
	}
	slices.Sort(out[min(1, len(out)):]) // keep <dir>/bin first, nested sorted
	return out
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
