package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

// enforceAndExec applies Landlock to this process and becomes the
// command — Landlock rules survive execve, which is the whole trick.
//
// BestEffort: the strictest ABI the kernel offers is applied; on
// kernels without Landlock the exec proceeds unrestricted (matching
// the library's contract). Network restriction needs ABI 4+ and covers
// classic TCP only. `doctor` reports the machine's actual ceiling.
func enforceAndExec(p Policy, argv []string, environ []string) error {
	if !p.WriteAll {
		roots := make([]landlock.Rule, 0, len(writeRoots(p))+1)
		roots = append(roots, landlock.RODirs("/"))
		for _, root := range writeRoots(p) {
			roots = append(roots, landlock.RWDirs(root).IgnoreIfMissing())
		}
		if err := landlock.V5.BestEffort().RestrictPaths(roots...); err != nil {
			return fmt.Errorf("sandbox: landlock paths: %w", err)
		}
	}
	if !p.Network {
		// No allow rules: all TCP bind/connect denied (ABI 4+).
		if err := landlock.V5.BestEffort().RestrictNet(); err != nil {
			return fmt.Errorf("sandbox: landlock net: %w", err)
		}
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	return unix.Exec(path, argv, environ)
}

// Available reports the enforcement backend for status displays.
func Available() string {
	data, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil || !strings.Contains(string(data), "landlock") {
		return "unenforced — kernel without Landlock (best-effort no-op)"
	}
	return "Linux Landlock (best-effort by kernel ABI)"
}
