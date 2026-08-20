// Command koi-acp is the AIProvider plugin for any ACP agent (#167,
// role 1).
//
// koi-claude drives one vendor's CLI. This drives a *protocol*: any
// agent that speaks the Agent Client Protocol — and there were 25+ by
// early 2026, with Zed, JetBrains, Neovim, VS Code and Microsoft's
// Intelligent Terminal as clients — answers `??` and `explain` with no
// koi-specific code on either side.
//
// It is a plugin, and it is deletable if ACP does not take. The
// *outbound* role (an agent executing inside koi) is core instead,
// because a plugin may never hold an exec channel (#34) — so this one
// deliberately declines the terminal capability. The `??` surface
// composes a command for the user to read; it does not run anything,
// and a provider that could would be a different feature with a
// different trust story.
//
// The agent to drive is $KOI_ACP_AGENT (a command line), defaulting to
// `claude-code-acp`.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/blairham/koi-shell/internal/acp"
	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
	pluginsdk "github.com/blairham/koi-shell/pkg/pluginsdk/v1"
)

type info struct {
	pluginapi.UnimplementedPluginInfoServer
	caps []pluginapi.Capability
}

func (i info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:         "koi-acp",
		Version:      "0.1.0",
		Capabilities: i.caps,
	}, nil
}

type ai struct {
	pluginapi.UnimplementedAIProviderServer
}

func (ai) Compose(req *pluginapi.ComposeRequest, stream pluginapi.AIProvider_ComposeServer) error {
	out, err := ask(stream.Context(), composePrompt(req))
	if err != nil {
		return err
	}
	command, explanation := splitCandidate(out)
	if command == "" {
		return fmt.Errorf("the agent returned no command")
	}
	return stream.Send(&pluginapi.ComposeCandidate{
		Command:     command,
		Explanation: explanation,
		Final:       true,
	})
}

func (ai) Explain(ctx context.Context, req *pluginapi.ExplainRequest) (*pluginapi.ExplainResponse, error) {
	out, err := ask(ctx, explainPrompt(req))
	if err != nil {
		return nil, err
	}
	return &pluginapi.ExplainResponse{Explanation: strings.TrimSpace(out)}, nil
}

// ask runs one turn against the agent and returns everything it said.
//
// A fresh agent per call, deliberately. A resident one would be faster
// and would also carry the last question's context into the next one,
// which for `??` is wrong: each composition is its own request, and a
// shell that quietly threads them is a shell whose suggestions depend
// on what you asked an hour ago. The host's deadline (#20's AITimeout)
// travels in ctx and kills the process with it.
func ask(ctx context.Context, prompt string) (string, error) {
	argv := agentArgv()
	// nil runner: no terminal capability. This provider composes text
	// for the user to read, and nothing here may execute.
	//
	// The agent's diagnostics stay discarded here — this runs behind the
	// prompt, where interleaving another process's stderr with the edit
	// line is its own damage — unless KOI_DEBUG asks for them (#331).
	var stderr io.Writer
	if os.Getenv("KOI_DEBUG") != "" {
		stderr = os.Stderr
	}
	client, err := acp.Start(ctx, argv, nil, stderr)
	if err != nil {
		return "", fmt.Errorf("starting %s: %w", strings.Join(argv, " "), err)
	}
	defer client.Close() //nolint:errcheck // teardown

	var mu sync.Mutex
	var said strings.Builder
	client.OnUpdate = func(u acp.Update) {
		if u.Kind != "agent_message_chunk" {
			return
		}
		mu.Lock()
		said.WriteString(u.Text)
		mu.Unlock()
	}

	if _, err := client.Initialize(ctx); err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if _, err := client.NewSession(ctx, cwd); err != nil {
		return "", err
	}
	stop, err := client.Prompt(ctx, prompt)
	if err != nil {
		return "", err
	}
	mu.Lock()
	text := said.String()
	mu.Unlock()
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("the agent said nothing (stopped: %s)", stop)
	}
	return text, nil
}

func agentArgv() []string {
	if cmd := strings.Fields(os.Getenv("KOI_ACP_AGENT")); len(cmd) > 0 {
		return cmd
	}
	return []string{"claude-code-acp"}
}

func composePrompt(req *pluginapi.ComposeRequest) string {
	var b strings.Builder
	b.WriteString("You translate a request into exactly one shell command.\n")
	b.WriteString("Reply with the command on the first line — no code fences, no prose, no leading $.\n")
	b.WriteString("Optionally add ONE short rationale line starting with '# ' after it.\n")
	b.WriteString("Do not run anything; just answer.\n\n")
	writeContext(&b, req.GetContext())
	b.WriteString("Request: " + req.GetQuery() + "\n")
	return b.String()
}

func explainPrompt(req *pluginapi.ExplainRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Explain in 2-3 sentences why this shell command exited %d (or what it does):\n\n", req.GetExitCode())
	b.WriteString("  " + req.GetCommand() + "\n\n")
	writeContext(&b, req.GetContext())
	b.WriteString("Reply with plain prose only. Do not run anything.\n")
	return b.String()
}

// writeContext folds in what the host chose to send. It is already
// scrubbed and allowlisted (#20): recent commands come from a history
// store that never records a secret-bearing command, and the
// environment is an allowlist. The plugin adds nothing.
func writeContext(b *strings.Builder, sc *pluginapi.ShellContext) {
	if sc == nil {
		return
	}
	if sc.GetCwd() != "" {
		b.WriteString("Working directory: " + sc.GetCwd() + "\n")
	}
	if recent := sc.GetRecentCommands(); len(recent) > 0 {
		b.WriteString("Recent commands:\n")
		for _, c := range recent {
			b.WriteString("  " + c + "\n")
		}
	}
	b.WriteString("\n")
}

// splitCandidate takes the first non-empty line as the command and any
// following comment line as the rationale — the same shape koi-claude
// asks for, because the shell's parsing is the same either way.
func splitCandidate(out string) (command, explanation string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#"):
			if command != "" && explanation == "" {
				explanation = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			}
		case command == "":
			// A fenced reply is common enough to be worth surviving.
			command = strings.Trim(line, "`")
		}
	}
	return command, explanation
}

// newPlugin wires the services this binary serves. main and the tests build
// it through here, so a capability cannot be claimed in Describe without the
// service behind it actually being registered.
func newPlugin() pluginsdk.Plugin {
	p := pluginsdk.Plugin{AI: ai{}}
	p.Info = info{caps: pluginsdk.Capabilities(p)}
	return p
}

func main() { pluginsdk.Serve(newPlugin()) }
