// Command gish-git is the flagship tier-2 plugin: a gitstatusd-class
// prompt segment. Resident per-repo cache, fsnotify invalidation on the
// .git directory (never polls), background refreshes — a render answers
// from cache in microseconds, well inside the 50ms budget.
package main

import (
	"context"

	pluginapi "github.com/blairham/gish/pkg/pluginapi/v1"
	pluginsdk "github.com/blairham/gish/pkg/pluginsdk/v1"
)

type info struct {
	pluginapi.UnimplementedPluginInfoServer
	caps []pluginapi.Capability
}

func (i info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:         "gish-git",
		Version:      "0.1.0",
		Capabilities: i.caps,
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

// newPlugin wires the services this binary serves. main and the tests build
// it through here, so a capability cannot be claimed in Describe without the
// service behind it actually being registered.
func newPlugin() pluginsdk.Plugin {
	p := pluginsdk.Plugin{Prompt: prompt{cache: newRepoCache()}}
	p.Info = info{caps: pluginsdk.Capabilities(p)}
	return p
}

func main() { pluginsdk.Serve(newPlugin()) }
