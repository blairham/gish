// Package remote brings gish to a box you only have ssh to (#98).
//
// The pitch is the 2AM incident host: a single static Go binary is
// scp-able, but nobody does that by hand. `gish ssh host` probes the
// remote, drops a content-addressed copy of gish where it is allowed to
// run, and execs an interactive session — and falls back to plain ssh
// the instant anything about that is not true.
//
// Two invariants shape every file in this package:
//
//   - **Never install.** No chsh, no remote dotfile edits, no daemon.
//     A ~/.bashrc hook that auto-launches gish is the obvious shortcut
//     and it is forbidden: it breaks rsync and scp the day the remote rc
//     prints one byte.
//   - **Every failure lands in plain ssh**, with one line on stderr.
//     The feature is negative value if it ever makes getting a shell
//     slower than not having it.
package remote

import (
	"context"
	"io"
	"time"
)

// waitDelay bounds how long a killed command's pipes are drained after
// its context ends. Small, because it only ever elapses when a
// grandchild has outlived its parent and is holding a pipe open — the
// wedged-remote case the deadlines exist for.
const waitDelay = 500 * time.Millisecond

// Transport runs commands on the far side. Factoring this out from the
// first commit buys two things immediately: `kubectl exec` and `docker
// exec` are the same problem with a different verb, and a local `sh`
// transport makes the whole probe/push/verify/fallback matrix testable
// with no ssh, no network, and no remote host.
type Transport interface {
	// Target names the destination for messages ("user@host").
	Target() string

	// Run executes a /bin/sh command line, feeding it stdin (which may
	// be nil) and returning its standard output. Standard error is
	// folded into the returned error, since every caller here treats a
	// failed remote command as a reason to degrade, not to report.
	Run(ctx context.Context, script string, stdin io.Reader) ([]byte, error)

	// Interactive hands the terminal to a remote command and blocks
	// until it exits. The transport owns the pty (for ssh, `-t`), so
	// window-size and signal forwarding are its problem, not ours.
	Interactive(ctx context.Context, command string) error

	// Close releases transport-level resources (for ssh, the multiplexed
	// control connection). It never fails the caller's operation.
	Close() error
}
