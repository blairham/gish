// Command gish is an interactive shell: zsh's interactive experience,
// bash's ubiquity, and a native, contract-first plugin system.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/repl"
	"github.com/blairham/gish/internal/sandbox"
)

// Stamped by -ldflags at build/release time; see Makefile and .goreleaser.yaml.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	// Private re-exec mode for sandboxed commands (#21): the parent
	// rewrote argv to [gish, __sandbox-exec, policy, --, cmd…]. Handled
	// before flag parsing — it is not CLI surface.
	if len(os.Args) >= 2 && os.Args[1] == sandbox.ExecFlag {
		return runSandboxExec(os.Args[2:])
	}

	// `gish ssh host` (#98). Intercepted before flag parsing because the
	// flags after it belong to ssh, not to gish. A script literally named
	// `ssh` is still runnable as `gish ./ssh`.
	if len(os.Args) >= 2 && os.Args[1] == "ssh" {
		return runSSH(context.Background(), os.Args[2:])
	}
	// `gish acp` hosts an ACP agent, running its commands through gish's
	// sandbox and deadlines (#167). A subcommand for the same reason
	// `gish ssh` is one: it owns the terminal for its whole run.
	if len(os.Args) >= 2 && os.Args[1] == "acp" {
		if err := repl.RunACP(context.Background(), os.Stdin, os.Stdout, os.Stderr, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "gish acp:", err)
			return 1
		}
		return 0
	}
	// `gish migrate` reads an existing bash/zsh setup (#160). A
	// subcommand rather than only a builtin, because the moment it
	// matters most is before anyone has started a gish session.
	if len(os.Args) >= 2 && os.Args[1] == "migrate" {
		if err := repl.RunMigrate(os.Stdout, os.Stderr, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "gish migrate:", err)
			return 1
		}
		return 0
	}

	command := flag.String("c", "", "run `command` and exit")
	loginFlag := flag.Bool("l", false, "act as a login shell (source profile files)")
	sandboxFlag := flag.String("sandbox", "", "run every external command under sandbox `profile`")
	showVersion := flag.Bool("version", false, "print version and exit")
	// The far side of `gish ssh`: gish was copied to this host, not
	// installed on it. --rc names the pushed settings bundle; passing a
	// path (never the contents) keeps the settings out of argv, which is
	// world-readable through /proc.
	remoteSession := flag.Bool("remote-session", false, "this session was brought here by `gish ssh`")
	// Session restore (#103). The value is a session id or a unique
	// prefix; `sessions` lists them.
	restore := flag.String("restore", "", "start in the directory of recorded session `id`")
	rcPath := flag.String("rc", "", "read startup settings from `file`")
	flag.Parse()

	// The session reports this as GISH_VERSION (#120).
	repl.Version = version

	if *rcPath != "" {
		os.Setenv("GISH_RC", *rcPath) //nolint:errcheck // process-local
	}
	if *remoteSession {
		os.Setenv("GISH_REMOTE_SESSION", "1") //nolint:errcheck // process-local
	}
	if *restore != "" {
		// Read by the interactive loop once its runner exists: landing
		// somewhere needs the runner, and the environment is proposed
		// through the trust flow rather than applied here.
		os.Setenv("GISH_RESTORE_SESSION", *restore) //nolint:errcheck // process-local
	}

	if *showVersion {
		fmt.Printf("gish %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}
	// The agent entry point: a session launched through a name beginning
	// `gish-agent` is sandboxed by default, because the caller is a
	// program rather than a person and nobody is there to read a warning.
	// A symlink is the whole install — one binary, and the invocation name
	// carries the posture, the way argv[0] already carries login (#41).
	// An explicit --sandbox always wins, including `--sandbox none`.
	if *sandboxFlag == "" {
		*sandboxFlag = agentSandboxProfile(os.Args[0])
	}
	if *sandboxFlag != "" && *sandboxFlag != "none" {
		if err := repl.SetSessionSandbox(*sandboxFlag); err != nil {
			fmt.Fprintln(os.Stderr, "gish:", err)
			return 2
		}
	}

	// Login invocation (#41): the -l flag, or argv[0] beginning with
	// '-' — how login(1) and sshd invoke a user's shell.
	login := *loginFlag || strings.HasPrefix(os.Args[0], "-")

	ctx := context.Background()
	var err error
	switch {
	case *command != "":
		// Everything after the command string is $0 then $1…
		err = repl.RunCommand(ctx, *command, login, flag.Args()...)
	case flag.NArg() > 0:
		err = repl.RunFile(ctx, flag.Arg(0), login, flag.Args()[1:]...)
	default:
		err = repl.Run(ctx, login)
	}
	if err == nil {
		return 0
	}
	// A nonzero exit status is the script's exit code, not a gish error.
	if status, ok := errors.AsType[interp.ExitStatus](err); ok {
		return int(status)
	}
	fmt.Fprintln(os.Stderr, "gish:", err)
	return 1
}

// agentSandboxDefault is the profile an agent-named invocation gets.
// workspace is the one that matches how an agent already works — it
// writes in the tree it was pointed at — while leaving network and
// environment alone, so the shell is confined without the tools inside
// it appearing broken.
const agentSandboxDefault = "workspace"

// agentSandboxProfile returns the profile implied by the invocation
// name, or "" when the name implies nothing.
//
// The name is matched after stripping the directory and the login dash,
// so /usr/local/bin/gish-agent and -gish-agent both count. A suffix is
// allowed because harnesses that pick a shell by grepping their $SHELL
// for "bash" or "zsh" need it in the name — `gish-agent-bash` is that
// spelling, and refusing it would push people back to a wrapper script.
func agentSandboxProfile(argv0 string) string {
	name := strings.TrimPrefix(filepath.Base(argv0), "-")
	if name == "gish-agent" || strings.HasPrefix(name, "gish-agent-") {
		return agentSandboxDefault
	}
	return ""
}

// runSandboxExec enforces the policy and becomes the command; it only
// returns on failure. 126 matches "found but cannot execute".
func runSandboxExec(args []string) int {
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(os.Stderr, "gish: sandbox: malformed re-exec invocation")
		return 126
	}
	if err := sandbox.Exec(args[0], args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "gish:", err)
		return 126
	}
	return 0 // unreachable: Exec replaces the process
}
