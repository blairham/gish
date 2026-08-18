package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/blairham/koi-shell/internal/remote"
	"github.com/blairham/koi-shell/internal/term"
)

// `koi ssh [user@]host` (#98): the answer to the most-cited blocker to
// adopting any new shell — "my shell has to be everywhere I ssh".
//
// The subcommand is explicit and does not shadow `ssh`. Auto-hijacking
// it would drop executables on servers people do not own, which trips
// file-integrity monitoring and violates change control at plenty of
// shops. Explicit is also honest: the user asked for their shell to
// follow them.

const sshUsage = `usage: koi ssh [ssh-flags] [user@]host
       koi ssh --uninstall [user@]host

Bring koi to a host you only have ssh to: probe it, copy one static
binary plus your prompt settings into a cache dir under your home
there, and open an interactive koi. Repeat visits copy nothing.

  --uninstall   remove everything koi left on the host, then exit
  --ephemeral   wipe the dropped files when the session ends
  --forget      forget the remembered answer for this host, then ask again

Anything else is passed through to ssh (-p, -J, -i, …).

Whether to bring koi is KOI_SSH_BRING: ask (default, remembered per
host) | always | never. Every failure falls back to plain ssh.`

// runSSH is the subcommand. It returns a process exit code and, by
// design, almost never fails: the landing zone for every problem is
// plain ssh, so the user gets a shell either way.
func runSSH(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stdout, sshUsage)
		return 0
	}

	inv := parseSSHArgs(args)
	host, sshArgs := inv.host, inv.sshArgs
	uninstall, ephemeral, forget := inv.uninstall, inv.ephemeral, inv.forget
	if host == "" {
		fmt.Fprintln(os.Stderr, "koi ssh: no host given\n"+sshUsage)
		return 2
	}

	transport := remote.NewSSH(host, sshArgs)
	defer transport.Close() //nolint:errcheck // best-effort teardown

	if uninstall {
		removed, err := remote.Uninstall(ctx, transport)
		if err != nil {
			fmt.Fprintln(os.Stderr, "koi ssh:", err)
			return 1
		}
		if len(removed) == 0 {
			fmt.Fprintf(os.Stderr, "koi ssh: nothing of koi's on %s\n", host)
		}
		for _, d := range removed {
			fmt.Fprintf(os.Stderr, "koi ssh: removed %s:%s\n", host, d)
		}
		if err := remote.Forget(host); err != nil {
			fmt.Fprintln(os.Stderr, "koi ssh:", err)
		}
		return 0
	}
	if forget {
		if err := remote.Forget(host); err != nil {
			fmt.Fprintln(os.Stderr, "koi ssh:", err)
			return 1
		}
	}

	sess, err := remote.Bring(ctx, transport, remote.Options{
		Host:        host,
		SSHArgs:     sshArgs,
		Mode:        os.Getenv("KOI_SSH_BRING"),
		Interactive: term.IsTerminal(os.Stdin),
		Ephemeral:   ephemeral,
		Stderr:      os.Stderr,
		Stdin:       os.Stdin,
	})
	if err != nil {
		// One line, then a normal ssh. If bringing koi along ever makes
		// getting a shell slower or less reliable than not having the
		// feature, the feature is negative value.
		fmt.Fprintf(os.Stderr, "koi ssh: %v; using plain ssh\n", err)
		return exitCodeOf(remote.Fallback(host, sshArgs))
	}

	if sess.Pushed {
		fmt.Fprintf(os.Stderr, "koi ssh: copied koi to %s:%s (%s/%s)\n",
			host, sess.Probe.Dir, sess.Probe.OS, sess.Probe.Arch)
	}
	return exitCodeOf(transport.Interactive(ctx, sess.Command))
}

// sshInvocation is one parsed `koi ssh` command line: koi's own flags
// separated out, everything else left for ssh.
type sshInvocation struct {
	host      string
	sshArgs   []string
	uninstall bool
	ephemeral bool
	forget    bool
}

// parseSSHArgs splits koi's flags from ssh's. koi claims only long
// names that ssh does not use, so passthrough stays total.
func parseSSHArgs(args []string) sshInvocation {
	var inv sshInvocation
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--uninstall":
			inv.uninstall = true
		case a == "--ephemeral":
			inv.ephemeral = true
		case a == "--forget":
			inv.forget = true
		case strings.HasPrefix(a, "-"):
			inv.sshArgs = append(inv.sshArgs, a)
			// A flag that takes a separate value swallows the next
			// argument, or `koi ssh -o ConnectTimeout=1 host` would read
			// the option value as the hostname.
			if takesValue(a) && i+1 < len(args) {
				i++
				inv.sshArgs = append(inv.sshArgs, args[i])
			}
		case inv.host == "":
			inv.host = a
		default:
			// A command after the host, which ssh would run remotely.
			inv.sshArgs = append(inv.sshArgs, a)
		}
	}
	return inv
}

// sshValueFlags are ssh's single-letter options that take a separate
// argument. From ssh(1); kept as a set rather than guessed, because
// getting one wrong silently mangles the host.
var sshValueFlags = map[string]bool{
	"-B": true, "-b": true, "-c": true, "-D": true, "-E": true, "-e": true,
	"-F": true, "-I": true, "-i": true, "-J": true, "-L": true, "-l": true,
	"-m": true, "-O": true, "-o": true, "-P": true, "-p": true, "-Q": true,
	"-R": true, "-S": true, "-W": true, "-w": true,
}

// takesValue reports whether flag consumes the next argument. Attached
// forms (`-p2222`, `-oFoo=bar`) carry their own value, so only the bare
// flag swallows one.
func takesValue(flag string) bool { return sshValueFlags[flag] }

// exitCodeOf turns a child's result into our exit code, so `koi ssh`
// is as transparent to scripts as ssh itself.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "koi ssh:", err)
	return 1
}
