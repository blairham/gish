// Command gish-aws is the AWS plugin (#79): one resident process, four
// capabilities on one connection (the gish-git pattern) — the %p{aws}
// prompt segment (profile/region/SSO expiry from local files only),
// profile/region completion, per-directory AWS_PROFILE proposals
// through the #12 trust flow, and aws-whoami/aws-login commands.
//
// Credential posture: the plugin reads config *structure* and token
// *metadata* (names, regions, expiry timestamps) — never credential
// values — and never calls AWS on the prompt path.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-plugin"

	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

type info struct {
	pluginapi.UnimplementedPluginInfoServer
}

func (info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:    "gish-aws",
		Version: "0.1.0",
		Capabilities: []pluginapi.Capability{
			pluginapi.Capability_CAPABILITY_PROMPT_SEGMENT,
			pluginapi.Capability_CAPABILITY_COMPLETION,
			pluginapi.Capability_CAPABILITY_ENV,
			pluginapi.Capability_CAPABILITY_COMMAND,
		},
	}, nil
}

// --- prompt segment ---

type segment struct {
	pluginapi.UnimplementedPromptSegmentProviderServer
	cfg *loader
}

func (s *segment) Segments(context.Context, *pluginapi.SegmentsRequest) (*pluginapi.SegmentsResponse, error) {
	return &pluginapi.SegmentsResponse{Segments: []*pluginapi.SegmentDescriptor{{
		Id:          "aws",
		Description: "active AWS profile, region, and SSO token expiry",
		BudgetMs:    50,
		EnvKeys:     []string{"AWS_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION"},
	}}}, nil
}

func (s *segment) Render(_ context.Context, req *pluginapi.RenderRequest) (*pluginapi.RenderResponse, error) {
	return &pluginapi.RenderResponse{
		Text:  segmentText(s.cfg.Load(), s.cfg.home, req.GetEnv(), time.Now()),
		TtlMs: 2000,
	}, nil
}

// segmentText is the pure render: profile@region plus SSO freshness.
// Empty when AWS is not set up here — a segment must know when to shut
// up.
func segmentText(cfg *Config, home string, env map[string]string, now time.Time) string {
	profile := env["AWS_PROFILE"]
	if profile == "" {
		profile = "default"
	}
	prof, known := cfg.Profiles[profile]
	if !known && env["AWS_PROFILE"] == "" {
		return "" // no explicit profile, none configured: not an AWS box
	}
	region := env["AWS_REGION"]
	if region == "" {
		region = env["AWS_DEFAULT_REGION"]
	}
	if region == "" {
		region = prof.Region
	}
	text := "aws:" + profile
	if region != "" {
		text += "@" + region
	}
	if expiry := ssoExpiry(home, cfg.startURL(profile)); !expiry.IsZero() {
		if remaining := expiry.Sub(now); remaining <= 0 {
			text += " sso✗"
		} else {
			text += " sso:" + compactDuration(remaining)
		}
	}
	return text
}

func compactDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// --- completion ---

type completion struct {
	pluginapi.UnimplementedCompletionProviderServer
	cfg *loader
}

func (c *completion) Complete(req *pluginapi.CompleteRequest, stream pluginapi.CompletionProvider_CompleteServer) error {
	return stream.Send(&pluginapi.CompleteBatch{
		Candidates: completeLine(c.cfg.Load(), req.GetLine()),
		Final:      true,
	})
}

// completeLine offers what carapace cannot know: the user's own profile
// names after --profile, and regions after --region — only on aws
// command lines.
func completeLine(cfg *Config, line string) []*pluginapi.Candidate {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "aws" {
		return nil
	}
	partial := ""
	if !strings.HasSuffix(line, " ") && len(fields) > 1 {
		partial = fields[len(fields)-1]
		fields = fields[:len(fields)-1]
	}
	if len(fields) == 0 {
		return nil
	}
	var pool []string
	var desc string
	switch fields[len(fields)-1] {
	case "--profile":
		pool, desc = cfg.ProfileNames(), "aws profile"
	case "--region":
		pool, desc = awsRegions, "aws region"
	default:
		return nil
	}
	var out []*pluginapi.Candidate
	for _, name := range pool {
		if strings.HasPrefix(name, partial) {
			out = append(out, &pluginapi.Candidate{Value: name, Description: desc})
		}
	}
	return out
}

// --- env provider (.aws-profile through the #12 trust flow) ---

type env struct {
	pluginapi.UnimplementedEnvProviderServer
}

func (env) EnvDiff(_ context.Context, req *pluginapi.EnvDiffRequest) (*pluginapi.EnvDiffResponse, error) {
	dir, profile, region := findProfileFile(req.GetCwd())
	if dir == "" {
		return &pluginapi.EnvDiffResponse{}, nil
	}
	set := map[string]string{"AWS_PROFILE": profile}
	if region != "" {
		set["AWS_REGION"] = region
	}
	return &pluginapi.EnvDiffResponse{ForDir: dir, Set: set}, nil
}

// findProfileFile walks up for .aws-profile: `profile [region]` on the
// first non-comment line.
func findProfileFile(cwd string) (dir, profile, region string) {
	for d := cwd; ; d = filepath.Dir(d) {
		data, err := os.ReadFile(filepath.Join(d, ".aws-profile")) //nolint:gosec // repo-local marker file
		if err == nil {
			for line := range strings.Lines(string(data)) {
				fields := strings.Fields(line)
				if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
					continue
				}
				profile = fields[0]
				if len(fields) > 1 {
					region = fields[1]
				}
				return d, profile, region
			}
			return "", "", ""
		}
		if d == filepath.Dir(d) {
			return "", "", ""
		}
	}
}

// --- commands ---

type commands struct {
	pluginapi.UnimplementedCommandProviderServer

	mu         sync.Mutex
	whoamiAt   time.Time
	whoamiText string
}

const whoamiTTL = 5 * time.Minute

func (*commands) Commands(context.Context, *pluginapi.CommandsRequest) (*pluginapi.CommandsResponse, error) {
	return &pluginapi.CommandsResponse{Commands: []*pluginapi.CommandSpec{
		{Name: "aws-whoami", Summary: "sts get-caller-identity, cached 5m"},
		{Name: "aws-login", Summary: "aws sso login for the active profile"},
	}}, nil
}

func (c *commands) Run(stream pluginapi.CommandProvider_RunServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return sendExit(stream, 126)
	}
	switch start.GetName() {
	case "aws-whoami":
		return c.runWhoami(stream)
	case "aws-login":
		return runStreaming(stream, "aws", "sso", "login")
	}
	return sendExit(stream, 127)
}

func (c *commands) runWhoami(stream pluginapi.CommandProvider_RunServer) error {
	c.mu.Lock()
	if time.Since(c.whoamiAt) < whoamiTTL && c.whoamiText != "" {
		text := c.whoamiText
		c.mu.Unlock()
		_ = stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Stdout{Stdout: []byte(text)}}) //nolint:errcheck // best-effort stream
		return sendExit(stream, 0)
	}
	c.mu.Unlock()

	out, err := exec.CommandContext(stream.Context(), "aws", "sts", "get-caller-identity").Output()
	if err != nil {
		_ = stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Stderr{ //nolint:errcheck // best-effort stream
			Stderr: []byte("aws-whoami: " + err.Error() + "\n"),
		}})
		return sendExit(stream, 1)
	}
	c.mu.Lock()
	c.whoamiAt, c.whoamiText = time.Now(), string(out)
	c.mu.Unlock()
	_ = stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Stdout{Stdout: out}}) //nolint:errcheck // best-effort stream
	return sendExit(stream, 0)
}

func runStreaming(stream pluginapi.CommandProvider_RunServer, name string, args ...string) error {
	cmd := exec.CommandContext(stream.Context(), name, args...)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		_ = stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Stdout{Stdout: out}}) //nolint:errcheck // best-effort stream
	}
	if err != nil {
		return sendExit(stream, 1)
	}
	return sendExit(stream, 0)
}

func sendExit(stream pluginapi.CommandProvider_RunServer, code int32) error {
	return stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Exit{Exit: code}})
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gish-aws:", err)
		os.Exit(1)
	}
	cfg := newLoader(home)
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: pluginhost.Handshake,
		Plugins: map[string]plugin.Plugin{
			"info":       &pluginhost.InfoPlugin{Impl: info{}},
			"prompt":     &pluginhost.PromptPlugin{Impl: &segment{cfg: cfg}},
			"completion": &pluginhost.CompletionPlugin{Impl: &completion{cfg: cfg}},
			"env":        &pluginhost.EnvPlugin{Impl: env{}},
			"command":    &pluginhost.CommandPlugin{Impl: &commands{}},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
