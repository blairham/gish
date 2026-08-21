package repl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/sandbox"
)

// The sandbox surface (#21). Two shapes, one policy schema:
//
//	sandbox --profile readonly -- make test    one command under a profile
//	koi --sandbox readonly                    the whole session under one
//
// Both are argv rewrites to the re-exec child (internal/sandbox), so
// job control, terminal handoff, and signals see an ordinary process.
// A sandboxed session refuses per-command profiles: weakening is
// escalation, and platform sandboxes do not nest reliably — the #34
// agent gets an approval-gated escalation flow instead.

// sessionSandboxProfile names the session-wide policy ("" = none). Set
// once at startup by SetSessionSandbox, read by the exec middleware.
var sessionSandboxProfile string

// sessionSandboxWrite are paths added to whatever profile is in force.
//
// No profile is the right shape for a program that keeps state outside
// the tree it was pointed at: an agent writes the working directory,
// which is workspace, *and* a state directory under $HOME, which no
// profile allows. Adding paths is not a fifth profile because the need
// is per-installation rather than per-posture — the directory belongs to
// the harness, not to koi.
var sessionSandboxWrite []string

// SetSessionSandbox validates and installs the session-wide profile
// (the koi --sandbox flag).
func SetSessionSandbox(profile string) error {
	if _, err := sandbox.Resolve(profile, ""); err != nil {
		return err
	}
	sessionSandboxProfile = profile
	return nil
}

// SetSessionSandboxWrite installs extra writable paths for the session.
//
// Absolute only, and a relative path is an error rather than something
// resolved against the current directory: the caller is a harness
// writing a settings file, the shell's directory at the moment a command
// runs is not what it meant, and a path that quietly covered the wrong
// place would be the same silent-failure shape this exists to fix. A
// leading ~/ is expanded, because a settings file is not shell source
// and nothing else would expand it.
func SetSessionSandboxWrite(paths []string) error {
	var out []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(path, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("sandbox write path %q: %w", path, err)
			}
			path = filepath.Join(home, rest)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("sandbox write path %q is not absolute", path)
		}
		out = append(out, filepath.Clean(path))
	}
	sessionSandboxWrite = out
	return nil
}

// sandboxExecMiddleware wraps every external command in the session
// policy. It sits after builtins and plugin commands (in-process work
// is not exec) and after command-not-found (suggestions should see the
// real command name, not the wrapper).
func sandboxExecMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if sessionSandboxProfile == "" || len(args) == 0 {
			return next(ctx, args)
		}
		wrapped, err := wrapInSandbox(sessionSandboxProfile, interp.HandlerCtx(ctx).Dir, args)
		if err != nil {
			fmt.Fprintln(interp.HandlerCtx(ctx).Stderr, "koi: sandbox:", err)
			return interp.ExitStatus(126)
		}
		return next(ctx, wrapped)
	}
}

func wrapInSandbox(profile, cwd string, argv []string) ([]string, error) {
	policy, err := sandbox.Resolve(profile, cwd)
	if err != nil {
		return nil, err
	}
	// Extra paths widen a profile that already restricts writes; a
	// profile that allows all of them has nothing to add to.
	if !policy.WriteAll && len(sessionSandboxWrite) > 0 {
		policy.WritePaths = append(slices.Clone(policy.WritePaths), sessionSandboxWrite...)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return sandbox.WrapArgv(self, policy, argv)
}

const sandboxUsage = `usage: sandbox [--profile <name>] -- command [args…]

  sandbox                          show the sandbox status and profiles
  sandbox --profile readonly -- make test

profiles:
  readonly    write only the temp dir; network allowed
  workspace   write the working directory and temp dir; network allowed
  no-network  full filesystem; no TCP
  isolated    readonly + no network + allowlisted environment

Omitting --profile uses workspace. Enforcement: macOS Seatbelt, Linux
Landlock (best-effort by kernel ABI — doctor reports the ceiling).

environment:
  KOI_AGENT_PROFILE   profile for a koi-agent session (default workspace)
  KOI_SANDBOX_WRITE   extra writable paths, separated like $PATH`

// sandboxCallHandler intercepts `sandbox`, config-style: the rewrite
// hands the interpreter the wrapped argv and execution takes the
// normal path.
func sandboxCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "sandbox" {
			return next(ctx, args)
		}
		return runSandbox(interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runSandbox(hc interp.HandlerContext, args []string) []string {
	fail := func(err error) []string {
		hc.Errf("sandbox: %v\n", err)
		return []string{"false"}
	}
	if len(args) == 0 {
		if sessionSandboxProfile != "" {
			fmt.Fprintf(hc.Stdout, "session sandbox: %s\n", sessionSandboxProfile)
		} else {
			fmt.Fprintln(hc.Stdout, "session sandbox: off")
		}
		if len(sessionSandboxWrite) > 0 {
			fmt.Fprintf(hc.Stdout, "also writable: %s\n", strings.Join(sessionSandboxWrite, " "))
		}
		fmt.Fprintf(hc.Stdout, "profiles: %s\n", strings.Join(sandbox.ProfileNames(), " | "))
		fmt.Fprintf(hc.Stdout, "enforcement: %s\n", sandbox.Available())
		return []string{"true"}
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(hc.Stdout, sandboxUsage)
		return []string{"true"}
	}

	profile := "workspace"
	rest := args
	if rest[0] == "--profile" || rest[0] == "-p" {
		if len(rest) < 2 {
			return fail(fmt.Errorf("--profile needs a name\n%s", sandboxUsage))
		}
		profile = rest[1]
		rest = rest[2:]
	}
	if len(rest) == 0 || rest[0] != "--" {
		return fail(fmt.Errorf("missing `--` before the command\n%s", sandboxUsage))
	}
	rest = rest[1:]
	if len(rest) == 0 {
		return fail(fmt.Errorf("no command after `--`\n%s", sandboxUsage))
	}
	if sessionSandboxProfile != "" {
		return fail(fmt.Errorf("session already sandboxed under %q — per-command profiles are unavailable (platform sandboxes do not nest; escalation would defeat the session policy)",
			sessionSandboxProfile))
	}
	wrapped, err := wrapInSandbox(profile, hc.Dir, rest)
	if err != nil {
		return fail(err)
	}
	return wrapped
}
