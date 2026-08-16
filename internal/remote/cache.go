package remote

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Choosing which binary to send.
//
// The common case needs no cache at all: ssh'ing from a linux/amd64
// laptop to a linux/amd64 server means the binary to send is the one
// already running, and self-copy is both the fastest path and the one
// with no way to be out of date.
//
// Cross-platform needs a build for the far side, and gish deliberately
// does **not** fetch one. #112 settled the scope line — native for the
// keystroke, prompt, and cd path; delegate everything else — and a
// release downloader is a package manager's job, with a package
// manager's obligations (signatures, mirrors, proxies, revocation). So
// the cache is a directory the user fills, and the error says exactly
// how to fill it.

// The three facts about *this* process that decide whether a remote
// needs a different build. Vars rather than direct runtime references so
// a test can pose as a linux/amd64 laptop talking to an arm64 host
// without needing either machine.
var (
	goos     = func() string { return runtime.GOOS }
	goarch   = func() string { return runtime.GOARCH }
	selfPath = func() string {
		exe, err := os.Executable()
		if err != nil {
			return ""
		}
		return exe
	}
)

// BinCacheDir is where cross-platform gish builds live, one per target.
func BinCacheDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "gish", "remote-bin")
}

// BinaryFor returns the path of a gish executable that will run on the
// probed platform.
func BinaryFor(p Probe) (string, error) {
	if p.OS == goos() && p.Arch == goarch() {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("%w: %w", errNoBinary, err)
		}
		return filepath.EvalSymlinks(exe)
	}
	if p.OS == "windows" {
		// Native Windows interactive is sequenced to v1.x (#110), so
		// there is nothing honest to drop on a Windows host yet.
		return "", fmt.Errorf("%w: remote is Windows; WSL2 is the supported story (#110)", errNoBinary)
	}
	target := p.OS + "-" + p.Arch
	path := filepath.Join(BinCacheDir(), target, "gish")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%w for %s\n  build one:  GOOS=%s GOARCH=%s CGO_ENABLED=0 go build -ldflags='-s -w' -o %s ./cmd/gish\n  or drop a release binary there yourself",
			errNoBinary, target, p.OS, p.Arch, path)
	}
	return path, nil
}

// StaticCheck reports whether this binary is the pure-Go static build
// the whole feature assumes. `uname -sm` says "linux x86_64" and nothing
// about glibc versus musl, so a cgo-linked binary lands on Alpine and
// fails with an error that looks like the file is missing. The release
// build sets CGO_ENABLED=0 everywhere; this is the runtime reminder for
// anyone shipping a locally built binary.
func StaticCheck() (ok bool, detail string) {
	// A cgo-enabled build links the host's libc; the constant below is
	// set by the toolchain, so this is a compile-time truth reported at
	// runtime rather than an inspection of the file.
	if cgoEnabled {
		return false, "this gish was built with cgo, so it may not run on a remote with a different libc (Alpine/musl); rebuild with CGO_ENABLED=0"
	}
	return true, "pure-Go static build: portable across glibc and musl"
}
