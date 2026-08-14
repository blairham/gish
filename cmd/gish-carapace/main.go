// Command gish-carapace bridges the carapace completion registry
// (~1,000 CLIs) into gish as a tier-2 CompletionProvider — day-one
// completion breadth for the cost of one plugin (docs/plugins.md).
//
// It shells out to the resident user's carapace binary per request; the
// host's 80ms budget bounds the call, and a missing binary or
// unsupported command yields an empty final batch, never an error the
// user sees.
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
		Name:    "gish-carapace",
		Version: "0.1.0",
		Capabilities: []pluginapi.Capability{
			pluginapi.Capability_CAPABILITY_COMPLETION,
		},
	}, nil
}

type completion struct {
	pluginapi.UnimplementedCompletionProviderServer
	bridge *bridge
}

func (c completion) Complete(req *pluginapi.CompleteRequest, stream pluginapi.CompletionProvider_CompleteServer) error {
	words := splitLine(req.GetLine(), int(req.GetCursor()))
	values := c.bridge.complete(stream.Context(), words)

	limit := int(req.GetMaxCandidates())
	if limit == 0 || limit > len(values) {
		limit = len(values)
	}
	batch := &pluginapi.CompleteBatch{Final: true}
	for _, v := range values[:limit] {
		display := v.Display
		if display == "" {
			display = v.Value
		}
		batch.Candidates = append(batch.Candidates, &pluginapi.Candidate{
			Value:       v.Value,
			Display:     display,
			Description: v.Description,
		})
	}
	return stream.Send(batch)
}

func main() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: pluginhost.Handshake,
		Plugins: map[string]plugin.Plugin{
			"info":       &pluginhost.InfoPlugin{Impl: info{}},
			"completion": &pluginhost.CompletionPlugin{Impl: completion{bridge: newBridge()}},
			"prompt":     &pluginhost.PromptPlugin{},
			"history":    &pluginhost.HistoryPlugin{},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
