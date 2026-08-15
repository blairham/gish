package sandbox

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// sandboxExecPath is macOS's Seatbelt profile runner, present on every
// supported macOS. (The CLI is marked deprecated; the underlying
// Seatbelt mechanism is what the OS itself sandboxes apps with.)
const sandboxExecPath = "/usr/bin/sandbox-exec"

// enforceAndExec compiles the policy to an SBPL profile and becomes
// `sandbox-exec -p <profile> cmd …`.
func enforceAndExec(p Policy, argv []string, environ []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	full := append([]string{sandboxExecPath, "-p", sbpl(p), path}, argv[1:]...)
	return unix.Exec(sandboxExecPath, full, environ)
}

// sbpl renders the Seatbelt profile: default-allow, then deny the
// policy's restricted classes with carve-outs.
func sbpl(p Policy) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	if !p.WriteAll {
		b.WriteString("(deny file-write*)\n")
		for _, root := range writeRoots(p) {
			// Symlinked temp roots (/var → /private/var) must match both
			// spellings; Seatbelt compares resolved paths.
			fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", root)
			if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
				fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", resolved)
			}
		}
	}
	if !p.Network {
		b.WriteString("(deny network*)\n")
	}
	return b.String()
}

// Available reports the enforcement backend for status displays.
func Available() string {
	if _, err := exec.LookPath(sandboxExecPath); err != nil {
		return "unavailable — " + sandboxExecPath + " missing"
	}
	return "macOS Seatbelt (sandbox-exec)"
}
