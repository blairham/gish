// Command koi is an interactive shell: zsh's interactive experience,
// bash's ubiquity, and a native, contract-first plugin system.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/mcpserve"
	"github.com/blairham/koi-shell/internal/repl"
	"github.com/blairham/koi-shell/internal/sandbox"
	"github.com/blairham/koi-shell/internal/term"
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
	// Which signals were ignored when koi was handed control is
	// recorded before koi installs any handler of its own (#441).
	repl.CaptureIgnoredSignals()

	// Private re-exec mode for sandboxed commands (#21): the parent
	// rewrote argv to [koi, __sandbox-exec, policy, --, cmd…]. Handled
	// before flag parsing — it is not CLI surface.
	if len(os.Args) >= 2 && os.Args[1] == sandbox.ExecFlag {
		return runSandboxExec(os.Args[2:])
	}

	// `koi ssh host` (#98). Intercepted before flag parsing because the
	// flags after it belong to ssh, not to koi. A script literally named
	// `ssh` is still runnable as `koi ./ssh`.
	if len(os.Args) >= 2 && os.Args[1] == "ssh" {
		return runSSH(context.Background(), os.Args[2:])
	}
	// `koi acp` hosts an ACP agent, running its commands through koi's
	// sandbox and deadlines (#167). A subcommand for the same reason
	// `koi ssh` is one: it owns the terminal for its whole run.
	if len(os.Args) >= 2 && os.Args[1] == "acp" {
		if err := repl.RunACP(context.Background(), os.Stdin, os.Stdout, os.Stderr, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "koi acp:", err)
			return 1
		}
		return 0
	}
	// `koi mcp` bridges an MCP client's stdio to the newest session's
	// state socket (#473). A subcommand because agent harnesses spawn a
	// command and speak over its pipes — there is nowhere to put a
	// builtin in that contract.
	if len(os.Args) >= 2 && os.Args[1] == "mcp" {
		socket, err := mcpserve.FindSocket()
		if err == nil {
			err = mcpserve.Proxy(socket, os.Stdin, os.Stdout)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "koi mcp:", err)
			return 1
		}
		return 0
	}
	// `koi adopt` applies a repo's checked-in .koi.toml (#209). A
	// subcommand for the migrate reason: the moment it matters most is
	// a fresh clone, before anyone has a koi session open.
	if len(os.Args) >= 2 && os.Args[1] == "adopt" {
		if err := repl.RunAdopt(os.Stdin, os.Stdout, os.Stderr, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "koi adopt:", err)
			return 1
		}
		return 0
	}
	// `koi migrate` reads an existing bash/zsh setup (#160). A
	// subcommand rather than only a builtin, because the moment it
	// matters most is before anyone has started a koi session.
	if len(os.Args) >= 2 && os.Args[1] == "migrate" {
		if err := repl.RunMigrate(os.Stdout, os.Stderr, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "koi migrate:", err)
			return 1
		}
		return 0
	}

	// --rc is the far side of `koi ssh`: koi was copied to this host,
	// not installed on it, and --rc names the pushed settings bundle.
	// Passing a path (never the contents) keeps the settings out of argv,
	// which is world-readable through /proc. --restore takes a session id
	// or a unique prefix (#103); `sessions` lists them.
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		if !errors.Is(err, errHelp) {
			fmt.Fprintln(os.Stderr, "koi:", err)
		}
		fmt.Fprintln(os.Stderr, usage)
		if errors.Is(err, errHelp) {
			return 0
		}
		return 2
	}

	// The session reports this as KOI_VERSION (#120).
	repl.Version = version
	// `set` options given in argv apply to whatever session follows.
	repl.SetSessionOptions(opts.setFlags)
	repl.SetSessionShoptOptions(opts.shoptFlags)

	repl.SkipStartupFiles(opts.noRC, opts.noProfile)
	if opts.noEditing {
		// bash's --noediting: no line editor even on a terminal. koi
		// has one switch for that already, so the flag sets it rather
		// than growing a second path (#531).
		os.Setenv("KOI_EDIT_MODE", "none") //nolint:errcheck // process-local
	}
	if opts.rc != "" {
		os.Setenv("KOI_RC", opts.rc) //nolint:errcheck // process-local
	}
	if opts.remoteSession {
		os.Setenv("KOI_REMOTE_SESSION", "1") //nolint:errcheck // process-local
	}
	if opts.restore != "" {
		// Read by the interactive loop once its runner exists: landing
		// somewhere needs the runner, and the environment is proposed
		// through the trust flow rather than applied here.
		os.Setenv("KOI_RESTORE_SESSION", opts.restore) //nolint:errcheck // process-local
	}

	if opts.version {
		fmt.Printf("koi %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}
	// The agent entry point: a session launched through a name beginning
	// `koi-agent` is sandboxed by default, because the caller is a
	// program rather than a person and nobody is there to read a warning.
	// A symlink is the whole install — one binary, and the invocation name
	// carries the posture, the way argv[0] already carries login (#41).
	// An explicit --sandbox always wins, including `--sandbox none`.
	if opts.sandbox == "" {
		opts.sandbox = agentSandboxProfile(os.Args[0])
	}
	if opts.sandbox != "" && opts.sandbox != "none" {
		if err := repl.SetSessionSandbox(opts.sandbox); err != nil {
			fmt.Fprintln(os.Stderr, "koi:", err)
			return 2
		}
		if err := repl.SetSessionSandboxWrite(sandboxWritePaths()); err != nil {
			fmt.Fprintln(os.Stderr, "koi:", err)
			return 2
		}
	}

	// Login invocation (#41): the -l flag, or argv[0] beginning with
	// '-' — how login(1) and sshd invoke a user's shell.
	login := opts.login || strings.HasPrefix(os.Args[0], "-")

	ctx := context.Background()

	// `-n` is a syntax check, so it must not reach any of the run paths
	// below (#233). bash *ignores* -n for an interactive shell, and that
	// is copied deliberately rather than improved on: a shell that
	// refused to start because someone left -n in their terminal profile
	// would be worse than one that quietly runs.
	// bash ignores -n for an interactive shell, so an interactive session
	// falls through to the normal path below rather than exiting. Copied
	// deliberately: a shell that refused to start because -n was left in
	// someone's terminal profile would be worse than one that runs.
	if opts.noexec && (opts.haveCommand || len(opts.operands) > 0 || !term.IsTerminal(os.Stdin)) {
		switch {
		case opts.haveCommand:
			// "-c" is the name bash puts in the message for this case, and
			// the name is what appears before line:col.
			err = repl.CheckCommand(opts.command, "-c")
		case len(opts.operands) > 0:
			err = repl.CheckFile(opts.operands[0])
		default:
			err = repl.CheckStdin()
		}
		if err != nil {
			repl.ReportSyntaxError(os.Stderr, err)
			return repl.NoExecStatus
		}
		return 0
	}

	if opts.prettyPrint && len(opts.operands) > 0 {
		// bash's --pretty-print parses the script and prints it back,
		// running nothing. With -c it does nothing at all, which is why
		// this is gated on a file operand (#531).
		if err := repl.PrettyPrintFile(opts.operands[0]); err != nil {
			fmt.Fprintln(os.Stderr, "koi:", err)
			return 1
		}
		return 0
	}
	switch {
	case opts.haveCommand:
		// Everything after the command string is $0 then $1…
		err = repl.RunCommand(ctx, opts.command, login, opts.interactive, opts.operands...)
	case len(opts.operands) > 0:
		err = repl.RunFile(ctx, opts.operands[0], login, opts.interactive, opts.operands[1:]...)
	default:
		err = repl.Run(ctx, login, opts.interactive)
	}
	if err == nil {
		return 0
	}
	// A nonzero exit status is the script's exit code, not a koi error.
	if status, ok := errors.AsType[interp.ExitStatus](err); ok {
		return int(status)
	}
	fmt.Fprintln(os.Stderr, "koi:", err)
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
// so /usr/local/bin/koi-agent and -koi-agent both count. A suffix is
// allowed because harnesses that pick a shell by grepping their $SHELL
// for "bash" or "zsh" need it in the name — `koi-agent-bash` is that
// spelling, and refusing it would push people back to a wrapper script.
func agentSandboxProfile(argv0 string) string {
	name := strings.TrimPrefix(filepath.Base(argv0), "-")
	if name != "koi-agent" && !strings.HasPrefix(name, "koi-agent-") {
		return ""
	}
	// The environment is the only channel a harness that picks its shell
	// through $SHELL actually controls: there is no argv to add a flag
	// to, so without this the profile is whatever the default is and the
	// only way out is a wrapper script on PATH — the same wrapper the
	// koi-agent-bash spelling exists to avoid.
	//
	// An unknown value is rejected by SetSessionSandbox rather than
	// falling back here, because falling back would silently confine a
	// session differently from how it was asked to be.
	if profile := os.Getenv(agentProfileEnv); profile != "" {
		return profile
	}
	return agentSandboxDefault
}

// The two environment knobs, named apart from KOI_AGENT_SANDBOX, which
// governs the `agent` builtin's own steps rather than the session the
// shell starts in.
const (
	agentProfileEnv = "KOI_AGENT_PROFILE"
	sandboxWriteEnv = "KOI_SANDBOX_WRITE"
)

// sandboxWritePaths reads the extra writable paths for the session.
//
// Split on the list separator rather than on spaces: these are paths,
// and a directory with a space in it is ordinary on the platform this
// feature is used on most.
func sandboxWritePaths() []string {
	value := os.Getenv(sandboxWriteEnv)
	if value == "" {
		return nil
	}
	return filepath.SplitList(value)
}

// runSandboxExec enforces the policy and becomes the command; it only
// returns on failure. 126 matches "found but cannot execute".
func runSandboxExec(args []string) int {
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(os.Stderr, "koi: sandbox: malformed re-exec invocation")
		return 126
	}
	if err := sandbox.Exec(args[0], args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "koi:", err)
		return 126
	}
	return 0 // unreachable: Exec replaces the process
}
