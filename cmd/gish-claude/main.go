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
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	pluginapi "github.com/blairham/gish/pkg/pluginapi/v1"
	pluginsdk "github.com/blairham/gish/pkg/pluginsdk/v1"
)

type info struct {
	pluginapi.UnimplementedPluginInfoServer
	caps []pluginapi.Capability
}

func (i info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:         "gish-claude",
		Version:      "0.1.0",
		Capabilities: i.caps,
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

func (ai) Plan(ctx context.Context, req *pluginapi.PlanRequest) (*pluginapi.PlanResponse, error) {
	out, err := runClaude(ctx, planPrompt(req))
	if err != nil {
		return nil, err
	}
	plan, err := parsePlan(out)
	if err != nil {
		return nil, err
	}
	return plan, nil
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

func planPrompt(req *pluginapi.PlanRequest) string {
	var b strings.Builder
	b.WriteString("Plan this multi-step shell task. Reply with ONLY a JSON object, no fences:\n")
	b.WriteString(`{"summary":"one paragraph","steps":[{"title":"intent","command":"shell command","destructive":false}]}` + "\n")
	b.WriteString("Mark any step that deletes, overwrites, force-pushes, or changes permissions as destructive.\n\n")
	writeContext(&b, req.GetContext())
	b.WriteString("Task: " + req.GetTask() + "\n")
	return b.String()
}

// parsePlan decodes the model's JSON, tolerating fences it was told
// not to use.
func parsePlan(out string) (*pluginapi.PlanResponse, error) {
	trimmed := strings.TrimSpace(out)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	var decoded struct {
		Summary string `json:"summary"`
		Steps   []struct {
			Title       string `json:"title"`
			Command     string `json:"command"`
			Destructive bool   `json:"destructive"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(trimmed)), &decoded); err != nil {
		return nil, fmt.Errorf("claude returned unparsable plan: %w", err)
	}
	plan := &pluginapi.PlanResponse{Summary: decoded.Summary}
	for _, s := range decoded.Steps {
		if strings.TrimSpace(s.Command) == "" {
			continue
		}
		plan.Steps = append(plan.Steps, &pluginapi.PlanStep{
			Title: s.Title, Command: s.Command, Destructive: s.Destructive,
		})
	}
	return plan, nil
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

// newPlugin wires the services this binary serves. main and the tests build
// it through here, so a capability cannot be claimed in Describe without the
// service behind it actually being registered.
func newPlugin() pluginsdk.Plugin {
	p := pluginsdk.Plugin{AI: ai{}}
	p.Info = info{caps: pluginsdk.Capabilities(p)}
	return p
}

func main() { pluginsdk.Serve(newPlugin()) }
