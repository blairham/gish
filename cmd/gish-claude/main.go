// Command gish-claude is the reference AIProvider plugin (#20): model
// access through the user's claude CLI, so it reuses their existing
// authentication and model selection. Provider-agnostic by contract —
// swap this plugin for a direct-API or local-model one and the shell's
// ?? and explain surfaces are unchanged.
//
// The plugin is a translator, nothing more: the shell already scrubbed
// and allowlisted the context; the CLI call inherits the host deadline
// through the request context.
package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hashicorp/go-plugin"

	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

type info struct {
	pluginapi.UnimplementedPluginInfoServer
}

func (info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:         "gish-claude",
		Version:      "0.1.0",
		Capabilities: []pluginapi.Capability{pluginapi.Capability_CAPABILITY_AI},
	}, nil
}

type ai struct {
	pluginapi.UnimplementedAIProviderServer
}

func (ai) Compose(req *pluginapi.ComposeRequest, stream pluginapi.AIProvider_ComposeServer) error {
	prompt := composePrompt(req)
	out, err := runClaude(stream.Context(), prompt)
	if err != nil {
		return err
	}
	command, explanation := splitCandidate(out)
	if command == "" {
		return fmt.Errorf("claude returned no command")
	}
	return stream.Send(&pluginapi.ComposeCandidate{
		Command:     command,
		Explanation: explanation,
		Final:       true,
	})
}

func (ai) Explain(ctx context.Context, req *pluginapi.ExplainRequest) (*pluginapi.ExplainResponse, error) {
	out, err := runClaude(ctx, explainPrompt(req))
	if err != nil {
		return nil, err
	}
	return &pluginapi.ExplainResponse{Explanation: strings.TrimSpace(out)}, nil
}

// runClaude shells to the user's claude CLI; the context carries the
// host's deadline, so a hung call dies with the request.
func runClaude(ctx context.Context, prompt string) (string, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return "", fmt.Errorf("the claude CLI is not installed (this provider drives it)")
	}
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt, "--output-format", "text")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude: %w", err)
	}
	return string(out), nil
}

func composePrompt(req *pluginapi.ComposeRequest) string {
	var b strings.Builder
	b.WriteString("You translate a request into exactly one shell command.\n")
	b.WriteString("Reply with the command on the first line — no code fences, no prose, no leading $.\n")
	b.WriteString("Optionally add ONE short rationale line starting with '# ' after it.\n\n")
	writeContext(&b, req.GetContext())
	b.WriteString("Request: " + req.GetQuery() + "\n")
	return b.String()
}

func explainPrompt(req *pluginapi.ExplainRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Explain in 2-3 sentences why this shell command exited %d (or what it does):\n\n", req.GetExitCode())
	b.WriteString("  " + req.GetCommand() + "\n\n")
	writeContext(&b, req.GetContext())
	b.WriteString("Reply with plain prose only.\n")
	return b.String()
}

func writeContext(b *strings.Builder, sc *pluginapi.ShellContext) {
	if sc == nil {
		return
	}
	fmt.Fprintf(b, "OS: %s; cwd: %s\n", sc.GetOs(), sc.GetCwd())
	if recent := sc.GetRecentCommands(); len(recent) > 0 {
		n := min(len(recent), 5)
		b.WriteString("Recent commands (newest first): " + strings.Join(recent[:n], " ; ") + "\n")
	}
}

// splitCandidate separates the command line from an optional
// '# rationale' line, defensively stripping fences the model was told
// not to use.
func splitCandidate(out string) (command, explanation string) {
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "```"):
			continue
		case strings.HasPrefix(line, "#"):
			if command != "" && explanation == "" {
				explanation = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			}
		case command == "":
			command = strings.TrimPrefix(line, "$ ")
		}
	}
	return command, explanation
}

func main() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: pluginhost.Handshake,
		Plugins: map[string]plugin.Plugin{
			"info": &pluginhost.InfoPlugin{Impl: info{}},
			"ai":   &pluginhost.AIPlugin{Impl: ai{}},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
