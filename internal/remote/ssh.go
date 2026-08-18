package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The ssh transport. Everything here rides **one** connection and one
// authentication: ControlMaster multiplexing is not an optimization, it
// is the difference between a feature and three password prompts for
// anyone behind a bastion or holding a hardware key. This is the single
// detail most likely to sink the feature, so it is the first thing set
// up and the last thing torn down.

// SSH runs commands on a host over a multiplexed ssh connection.
type SSH struct {
	host    string
	opts    []string // shared connection options, incl. ControlPath
	ctlPath string
	extra   []string // user's own ssh flags, passed through
}

// NewSSH prepares a multiplexed connection to host. Nothing dials yet —
// the first Run brings the master up, and every later call reuses it.
// extra carries the user's own ssh flags (-p, -J, -i…) verbatim.
func NewSSH(host string, extra []string) *SSH {
	ctl := controlPath(host)
	opts := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + ctl,
		"-o", "ControlPersist=60",
	}
	return &SSH{host: host, opts: opts, ctlPath: ctl, extra: extra}
}

func (s *SSH) Target() string { return s.host }

// controlPath puts the socket somewhere private and short. Unix domain
// socket paths are capped near 104 bytes on macOS, so the name is a hash
// rather than the host — a long bastion-qualified name would otherwise
// fail to bind with an error nobody can read.
func controlPath(host string) string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "koi")
	_ = os.MkdirAll(dir, 0o700) //nolint:errcheck // ssh reports the real failure
	sum := sha256.Sum256([]byte(host))
	return filepath.Join(dir, "ctl-"+hex.EncodeToString(sum[:])[:16])
}

func (s *SSH) args(rest ...string) []string {
	out := append([]string{}, s.opts...)
	out = append(out, s.extra...)
	out = append(out, s.host)
	return append(out, rest...)
}

func (s *SSH) Run(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	// -C so ssh compresses the binary on the wire for us: depending on
	// remote gzip/zstd would mean depending on remote tooling, which is
	// exactly what a hardened box does not have.
	cmd := exec.CommandContext(ctx, "ssh", append([]string{"-C", "-o", "BatchMode=no"}, s.args("--", script)...)...) //nolint:gosec // args are ours; host is the user's own argument
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// Killing the process on deadline is not enough to return on
	// deadline: Wait also drains the stdout/stderr pipes, and a
	// grandchild that outlives ssh keeps the write end open, so Wait
	// blocks long past the timeout. WaitDelay caps that — without it the
	// probe's 2s budget is advisory, and "never slower than plain ssh"
	// stops being true exactly when the remote is wedged.
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("ssh %s: %w: %s", s.host, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func (s *SSH) Interactive(ctx context.Context, command string) error {
	// -t explicitly: ssh only allocates a pty by default when no command
	// is given, and we always give one.
	cmd := exec.CommandContext(ctx, "ssh", append([]string{"-t"}, s.args("--", command)...)...) //nolint:gosec // args are ours
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Close drops the multiplexed master. Best-effort: ControlPersist would
// retire it anyway, and a failure here must never fail the session the
// user just finished.
func (s *SSH) Close() error {
	cmd := exec.Command("ssh", append([]string{"-O", "exit"}, s.args()...)...) //nolint:gosec // args are ours
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	_ = cmd.Run() //nolint:errcheck // best-effort teardown
	return nil
}

// Fallback runs a plain `ssh host` with the terminal inherited — the
// landing zone for every failure in this package. It reuses whatever
// control socket is already authenticated, so degrading costs the user
// the features and nothing else.
func Fallback(host string, extra []string) error {
	args := append([]string{"ssh"}, extra...)
	args = append(args, host)
	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // the user's own ssh invocation
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
