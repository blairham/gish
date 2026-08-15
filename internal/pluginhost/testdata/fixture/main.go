// Command fixture is the hermetic test plugin: it serves PluginInfo,
// CompletionProvider, and PromptSegmentProvider over the real go-plugin
// transport. Rendering the segment id "crash" exits the process, which
// is how the host's crash-healing is exercised.
package main

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/hashicorp/go-plugin"

	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

type info struct {
	pluginapi.UnimplementedPluginInfoServer
}

func (info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:    "fixture",
		Version: "0.0.1-test",
		Capabilities: []pluginapi.Capability{
			pluginapi.Capability_CAPABILITY_COMPLETION,
			pluginapi.Capability_CAPABILITY_PROMPT_SEGMENT,
			pluginapi.Capability_CAPABILITY_HISTORY,
			pluginapi.Capability_CAPABILITY_COMMAND,
			pluginapi.Capability_CAPABILITY_THEME,
			pluginapi.Capability_CAPABILITY_ENV,
			pluginapi.Capability_CAPABILITY_AI,
		},
	}, nil
}

type completion struct {
	pluginapi.UnimplementedCompletionProviderServer
}

func (completion) Complete(req *pluginapi.CompleteRequest, stream pluginapi.CompletionProvider_CompleteServer) error {
	return stream.Send(&pluginapi.CompleteBatch{
		Candidates: []*pluginapi.Candidate{
			{Value: "fixture-" + req.GetLine(), Description: "from fixture"},
		},
		Final: true,
	})
}

type prompt struct {
	pluginapi.UnimplementedPromptSegmentProviderServer
}

func (prompt) Segments(context.Context, *pluginapi.SegmentsRequest) (*pluginapi.SegmentsResponse, error) {
	return &pluginapi.SegmentsResponse{
		Segments: []*pluginapi.SegmentDescriptor{
			{Id: "test", Description: "fixture segment", BudgetMs: 50},
		},
	}, nil
}

func (prompt) Render(_ context.Context, req *pluginapi.RenderRequest) (*pluginapi.RenderResponse, error) {
	if req.GetSegmentId() == "crash" {
		os.Exit(1)
	}
	return &pluginapi.RenderResponse{Text: "fixture-segment", TtlMs: 100}, nil
}

type theme struct {
	pluginapi.UnimplementedThemeProviderServer
}

func (theme) Themes(context.Context, *pluginapi.ThemesRequest) (*pluginapi.ThemesResponse, error) {
	return &pluginapi.ThemesResponse{
		Themes: []*pluginapi.ThemeDescriptor{
			{Name: "fixture-theme", Description: "fixture whole-prompt theme", BudgetMs: 50},
		},
	}, nil
}

func (theme) RenderPrompt(_ context.Context, req *pluginapi.RenderPromptRequest) (*pluginapi.RenderPromptResponse, error) {
	return &pluginapi.RenderPromptResponse{
		Prompt:     "fixture[" + req.GetContext().GetCwd() + "]> ",
		ContPrompt: "fixture| ",
		Rprompt:    "fixture-right",
	}, nil
}

type env struct {
	pluginapi.UnimplementedEnvProviderServer
}

func (env) EnvDiff(_ context.Context, req *pluginapi.EnvDiffRequest) (*pluginapi.EnvDiffResponse, error) {
	// Propose only for directories under a path containing "envdir";
	// everything else gets an empty response (no proposal).
	if !strings.Contains(req.GetCwd(), "envdir") {
		return &pluginapi.EnvDiffResponse{}, nil
	}
	return &pluginapi.EnvDiffResponse{
		ForDir: req.GetCwd(),
		Set:    map[string]string{"FIXTURE_ENV": "on", "LD_PRELOAD": "/evil.so"},
		Unset:  []string{"FIXTURE_OLD"},
	}, nil
}

type ai struct {
	pluginapi.UnimplementedAIProviderServer
}

func (ai) Compose(req *pluginapi.ComposeRequest, stream pluginapi.AIProvider_ComposeServer) error {
	if err := stream.Send(&pluginapi.ComposeCandidate{
		Command:     "echo composed:" + req.GetQuery(),
		Explanation: "fixture rationale",
	}); err != nil {
		return err
	}
	return stream.Send(&pluginapi.ComposeCandidate{Command: "echo alternative", Final: true})
}

func (ai) Explain(_ context.Context, req *pluginapi.ExplainRequest) (*pluginapi.ExplainResponse, error) {
	return &pluginapi.ExplainResponse{
		Explanation: "fixture explains: " + req.GetCommand(),
	}, nil
}

type historyBackend struct {
	pluginapi.UnimplementedHistoryBackendServer
	mu      sync.Mutex
	entries []*pluginapi.HistoryEntry
}

func (h *historyBackend) Append(_ context.Context, req *pluginapi.AppendRequest) (*pluginapi.AppendResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, req.GetEntry())
	return &pluginapi.AppendResponse{Stored: true}, nil
}

func (h *historyBackend) Search(req *pluginapi.SearchRequest, stream pluginapi.HistoryBackend_SearchServer) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	batch := &pluginapi.SearchBatch{Final: true}
	for _, e := range h.entries {
		if strings.Contains(e.GetCommand(), req.GetQuery()) {
			batch.Entries = append(batch.Entries, e)
		}
	}
	return stream.Send(batch)
}

type commands struct {
	pluginapi.UnimplementedCommandProviderServer
}

func (commands) Commands(context.Context, *pluginapi.CommandsRequest) (*pluginapi.CommandsResponse, error) {
	return &pluginapi.CommandsResponse{Commands: []*pluginapi.CommandSpec{
		{Name: "fixture-echo", Summary: "echo args"},
		{Name: "fixture-upper", Summary: "uppercase stdin"},
		{Name: "fixture-fail", Summary: "exit 3"},
		{Name: "cd", Summary: "reserved name, must be rejected"},
	}}, nil
}

func (commands) Run(stream pluginapi.CommandProvider_RunServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Exit{Exit: 126}})
	}
	switch start.GetName() {
	case "fixture-echo":
		line := "echo:" + strings.Join(start.GetArgs(), ",") + " cwd:" + start.GetCwd() + "\n"
		_ = stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Stdout{Stdout: []byte(line)}})
		return stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Exit{Exit: 0}})
	case "fixture-upper":
		var data []byte
		for {
			in, err := stream.Recv()
			if err != nil {
				break
			}
			if in.GetStdinEof() {
				break
			}
			data = append(data, in.GetStdin()...)
		}
		_ = stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Stdout{Stdout: []byte(strings.ToUpper(string(data)))}})
		return stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Exit{Exit: 0}})
	case "fixture-fail":
		_ = stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Stderr{Stderr: []byte("deliberate failure\n")}})
		return stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Exit{Exit: 3}})
	}
	return stream.Send(&pluginapi.RunOutput{Output: &pluginapi.RunOutput_Exit{Exit: 127}})
}

func main() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: pluginhost.Handshake,
		Plugins: map[string]plugin.Plugin{
			"info":       &pluginhost.InfoPlugin{Impl: info{}},
			"command":    &pluginhost.CommandPlugin{Impl: commands{}},
			"completion": &pluginhost.CompletionPlugin{Impl: completion{}},
			"prompt":     &pluginhost.PromptPlugin{Impl: prompt{}},
			"history":    &pluginhost.HistoryPlugin{Impl: &historyBackend{}},
			"theme":      &pluginhost.ThemePlugin{Impl: theme{}},
			"env":        &pluginhost.EnvPlugin{Impl: env{}},
			"ai":         &pluginhost.AIPlugin{Impl: ai{}},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
