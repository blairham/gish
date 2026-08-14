package repl

import (
	"os"
	"path/filepath"
	"strings"
)

// brewPrefixes are the conventional Homebrew roots, most likely first.
var brewPrefixes = []string{
	"/opt/homebrew",              // macOS arm64
	"/usr/local",                 // macOS intel
	"/home/linuxbrew/.linuxbrew", // linux
}

// brewShellenv applies `brew shellenv` natively at interactive startup
// (#44): pure string and stat work, no brew subprocess, measured under
// the #37 budget. No-op when brew is absent or already on PATH — users
// with their own setup keep it.
func brewShellenv() {
	pathVar := os.Getenv("PATH")
	for _, prefix := range brewPrefixes {
		bin := filepath.Join(prefix, "bin")
		if _, err := os.Stat(filepath.Join(bin, "brew")); err != nil {
			continue
		}
		if onPath(pathVar, bin) {
			return // brew already reachable; nothing to do
		}
		os.Setenv("PATH", bin+string(os.PathListSeparator)+
			filepath.Join(prefix, "sbin")+string(os.PathListSeparator)+pathVar)
		os.Setenv("HOMEBREW_PREFIX", prefix)
		if os.Getenv("MANPATH") != "" {
			os.Setenv("MANPATH", filepath.Join(prefix, "share", "man")+
				string(os.PathListSeparator)+os.Getenv("MANPATH"))
		}
		return
	}
}

func onPath(pathVar, dir string) bool {
	for _, p := range strings.Split(pathVar, string(os.PathListSeparator)) {
		if p == dir {
			return true
		}
	}
	return false
}
