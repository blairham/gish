//go:build !unix

package acp

import (
	"os"
	"os/exec"
)

// Process groups are a unix concept; on Windows, killing a whole tree
// needs job objects, which track with the interactive port (#87). Until
// then the direct child is all a kill reaches, and WaitDelay (see
// ExecRunner) keeps a surviving descendant from hanging wait_for_exit.
func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(p *os.Process) error { return p.Kill() }
