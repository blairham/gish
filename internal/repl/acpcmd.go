package repl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/blairham/koi-shell/internal/acp"
	"github.com/blairham/koi-shell/internal/sandbox"
)

// `koi acp` — hosting an agent inside the shell (#167, role 2).
//
// The market evidence is about the execution substrate, not about shell
// users: every agent runner shells out with no sandbox, no deadline and
// no structured result. `just-bash` reached 4.77M npm downloads a month
// in eight months; OpenAI forks zsh's exec.c to intercept execve; VS
// Code shipped a `riskAssessment` that has an LLM *guess* what a command
// does. Meanwhile the claim that people are leaving zsh because agents
// assume bash is **not evidenced** — 2 HN accounts, and 0 of 827 in a
// Reddit corpus check. So this is built because the substrate is
// unserved, and it is deliberately not a marketing pillar (#169).
//
// What koi adds to ACP is exactly what ACP leaves out: the spec has no
// permission model, no sandboxing and no timeout. Every command an agent
// asks for here runs through the same sandbox profile machinery as
// `sandbox --profile workspace --`, and the agent sees an ordinary
// terminal id.
//
// This is core rather than a plugin because a plugin may never hold an
// exec channel (#34). That invariant is worth more than the convenience
// of shipping it outside the binary.

const acpUsage = `usage: koi acp [--profile PROFILE] [--] AGENT [ARGS...]

  koi acp claude-code-acp          host an agent; its commands run sandboxed
  koi acp --profile readonly AGENT  a stricter profile for the session
  koi acp --profile none AGENT      no sandbox (says so, loudly)

Type a prompt and press Enter. The agent's answer streams back; the
commands it runs go through koi's exec path with a sandbox profile and
a deadline, which is what ACP itself deliberately does not define.`

// RunACP hosts an ACP agent. It is a subcommand rather than a builtin
// for the same reason `koi ssh` is: it owns the terminal for its whole
// run, and it needs to work before anyone has a koi session open.
func RunACP(ctx context.Context, in io.Reader, out, errOut io.Writer, args []string) error {
	profile := "workspace"
	var argv []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--profile" && i+1 < len(args):
			i++
			profile = args[i]
		case a == "--help" || a == "-h":
			fmt.Fprintln(out, acpUsage)
			return nil
		case a == "--":
			argv = append(argv, args[i+1:]...)
			i = len(args)
		default:
			argv = append(argv, args[i:]...)
			i = len(args)
		}
	}
	if len(argv) == 0 {
		fmt.Fprintln(errOut, acpUsage)
		return errors.New("no agent command")
	}

	runner, describe, err := sandboxedRunner(profile)
	if err != nil {
		return err
	}

	client, err := acp.Start(ctx, argv, runner)
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck // teardown

	client.OnUpdate = func(u acp.Update) {
		switch u.Kind {
		case "agent_message_chunk":
			fmt.Fprint(out, u.Text)
		case "tool_call":
			// Every command the agent runs is visible as it happens.
			// Preview-before-execute is not available here — the agent
			// is executing, not proposing — so visibility is what stands
			// in for it, and hiding tool calls would be the one thing
			// that makes this worse than a terminal.
			fmt.Fprintf(out, "\n[%s] %s\n", u.Status, u.Title)
		}
	}

	// The handshake gets a deadline (#330). Deadlines on every call are
	// what koi adds to ACP, and the one place that had none was the
	// first exchange — where a mute agent (the wrong binary, a program
	// that is not an ACP agent at all) answers nothing and initialize
	// blocks forever. The prompt loop's calls stay unbounded on purpose:
	// a model can legitimately think for minutes, and Ctrl-C covers them.
	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	info, err := client.Initialize(hsCtx)
	if err != nil {
		return handshakeErr(hsCtx, err, argv[0])
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := client.NewSession(hsCtx, cwd); err != nil {
		return handshakeErr(hsCtx, err, argv[0])
	}
	fmt.Fprintf(errOut, "koi: hosting %s %s — commands run %s\n", info.Name, info.Version, describe)

	return promptLoop(ctx, client, in, out, errOut)
}

// handshakeTimeout bounds initialize + session/new. Generous, because a
// real agent may be cold-starting a runtime (claude-code-acp boots node
// and the claude CLI inside it) — but bounded, because an agent that
// says nothing in this long is not going to start saying something. A
// variable so the test does not wait it out.
var handshakeTimeout = 30 * time.Second

// handshakeErr turns a deadline expiry into an error that names the
// agent and the likely cause; any other handshake failure passes
// through as itself. context.DeadlineExceeded alone reads as koi timing
// out, when what happened is the agent never spoke.
func handshakeErr(ctx context.Context, err error, agent string) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("agent %q did not answer the ACP handshake within %s — is it an ACP agent?", agent, handshakeTimeout)
}

// promptLoop reads prompts and streams answers until EOF.
func promptLoop(ctx context.Context, client *acp.Client, in io.Reader, out, errOut io.Writer) error {
	reader := bufio.NewReader(in)
	for {
		fmt.Fprint(out, "\nagent> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Fprintln(out)
			return nil // EOF: the session is over
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		stop, err := client.Prompt(ctx, line)
		fmt.Fprintln(out)
		switch {
		case err != nil:
			fmt.Fprintln(errOut, "koi: acp:", err)
		case stop != "" && stop != "end_turn":
			// A turn that ended for another reason is worth saying:
			// "refusal" and "max_tokens" look identical to a user
			// otherwise, and they call for different next moves.
			fmt.Fprintln(errOut, "koi: agent stopped:", stop)
		}
	}
}

// sandboxedRunner wraps command execution in a sandbox profile, and
// describes what it did for the banner.
func sandboxedRunner(profile string) (acp.Runner, string, error) {
	if profile == "none" || profile == "off" {
		// Allowed, and said out loud. A sandbox the user turned off is
		// a decision; one that quietly did nothing is a lie.
		return acp.ExecRunner, "UNSANDBOXED (--profile none)", nil
	}
	// Resolve once, up front: an unknown profile must fail before an
	// agent is launched, not on the first command it tries to run.
	if _, err := sandbox.Resolve(profile, ""); err != nil {
		return nil, "", fmt.Errorf("%w\n%s", err, acpUsage)
	}
	return func(ctx context.Context, cmd acp.Command, out *acp.Output) (acp.Process, error) {
		wrapped := acp.Command{
			Command: cmd.Command,
			Args:    cmd.Args,
			Cwd:     cmd.Cwd,
			Env:     cmd.Env,
		}
		// The same rewrite `sandbox --profile p -- cmd` performs: koi
		// re-execs itself in the private sandbox mode, which then filters
		// the environment and applies the platform enforcement before
		// exec'ing the real command. The cwd is the agent's, since that
		// is the directory the workspace profile is about.
		argv, err := wrapInSandbox(profile, cmd.Cwd, append([]string{cmd.Command}, cmd.Args...))
		if err != nil {
			return nil, err
		}
		wrapped.Command, wrapped.Args = argv[0], argv[1:]
		return acp.ExecRunner(ctx, wrapped, out)
	}, "under the " + profile + " sandbox profile", nil
}
