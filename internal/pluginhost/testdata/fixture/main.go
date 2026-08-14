// Command fixture is the hermetic test plugin: it serves PluginInfo,
// CompletionProvider, and PromptSegmentProvider over the real go-plugin
// transport. Rendering the segment id "crash" exits the process, which
// is how the host's crash-healing is exercised.
package main

import (
	"context"
	"os"

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

func main() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: pluginhost.Handshake,
		Plugins: map[string]plugin.Plugin{
			"info":       &pluginhost.InfoPlugin{Impl: info{}},
			"completion": &pluginhost.CompletionPlugin{Impl: completion{}},
			"prompt":     &pluginhost.PromptPlugin{Impl: prompt{}},
			"history":    &pluginhost.HistoryPlugin{},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
