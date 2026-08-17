// Command gish is an interactive shell: zsh's interactive experience,
// bash's ubiquity, and a native, contract-first plugin system.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
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
	if *sandboxFlag != "" {
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
		err = repl.RunCommand(ctx, *command, login)
	case flag.NArg() > 0:
		err = repl.RunFile(ctx, flag.Arg(0), login)
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
