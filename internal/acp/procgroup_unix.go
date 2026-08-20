//go:build unix

package acp

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup starts the command in its own process group (#328).
func setProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the whole group. Setpgid made the child's
// pid its pgid, so the negative pid addresses every descendant still in
// the group — the wrapper, the shell under it, and whatever that forked.
func killProcessGroup(p *os.Process) error {
	err := syscall.Kill(-p.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		// ESRCH means the group is already gone, which is what a kill
		// is for; the spec keeps the terminal valid after one either way.
		return nil
	}
	// The group is unaddressable; at least stop the direct child.
	return p.Kill()
}
