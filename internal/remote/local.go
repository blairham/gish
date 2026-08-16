package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Local runs the same /bin/sh scripts through a local shell instead of
// over the network. It exists so the probe/push/verify/fallback matrix
// — noexec directories, read-only $HOME, missing sha256, truncated
// transfers — can be tested against a real POSIX shell under t.TempDir()
// with no ssh, no network, and no remote host, per the AGENTS.md rule
// that tests never touch real user state.
//
// It is a test seam, not a user-facing "run gish on localhost" feature.
type Local struct {
	// Env overrides the environment the scripts see. Setting HOME and
	// XDG_RUNTIME_DIR here is how a test steers the candidate-directory
	// chain into a temp dir.
	Env []string
	// Shell is the shell to run scripts with; defaults to /bin/sh.
	Shell string
	// Interacted records commands passed to Interactive, which a test
	// asserts on rather than actually running.
	Interacted []string
}

func (l *Local) Target() string { return "local" }

func (l *Local) shell() string {
	if l.Shell != "" {
		return l.Shell
	}
	return "/bin/sh"
}

func (l *Local) Run(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	cmd := exec.CommandContext(ctx, l.shell(), "-c", script) //nolint:gosec // scripts are ours; this is the test transport
	cmd.Env = l.Env
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = waitDelay // see SSH.Run: kill-on-deadline is not return-on-deadline
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("local: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Interactive records rather than executes: a test wants to assert on
// the exec line gish would have run, not hand a pty to a real shell.
func (l *Local) Interactive(_ context.Context, command string) error {
	l.Interacted = append(l.Interacted, command)
	return nil
}

func (l *Local) Close() error { return nil }
