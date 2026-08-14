// Command gish-git is the flagship tier-2 plugin: a gitstatusd-class
// prompt segment. Resident per-repo cache, fsnotify invalidation on the
// .git directory (never polls), background refreshes — a render answers
// from cache in microseconds, well inside the 50ms budget.
package main

import (
	"context"

	"github.com/hashicorp/go-plugin"

	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

type info struct {
	pluginapi.UnimplementedPluginInfoServer
}

func (info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:    "gish-git",
		Version: "0.1.0",
		Capabilities: []pluginapi.Capability{
			pluginapi.Capability_CAPABILITY_PROMPT_SEGMENT,
		},
	}, nil
}

type prompt struct {
	pluginapi.UnimplementedPromptSegmentProviderServer
	cache *repoCache
}

func (prompt) Segments(context.Context, *pluginapi.SegmentsRequest) (*pluginapi.SegmentsResponse, error) {
	return &pluginapi.SegmentsResponse{
		Segments: []*pluginapi.SegmentDescriptor{
			{Id: "git", Description: "git branch and status", BudgetMs: 50},
		},
	}, nil
}

func (p prompt) Render(ctx context.Context, req *pluginapi.RenderRequest) (*pluginapi.RenderResponse, error) {
	if req.GetSegmentId() != "git" {
		return &pluginapi.RenderResponse{}, nil
	}
	return &pluginapi.RenderResponse{
		Text:  p.cache.render(ctx, req.GetCwd()),
		TtlMs: 100,
	}, nil
}

func main() {
	cache := newRepoCache()
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: pluginhost.Handshake,
		Plugins: map[string]plugin.Plugin{
			"info":       &pluginhost.InfoPlugin{Impl: info{}},
			"prompt":     &pluginhost.PromptPlugin{Impl: prompt{cache: cache}},
			"completion": &pluginhost.CompletionPlugin{},
			"history":    &pluginhost.HistoryPlugin{},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
