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
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
