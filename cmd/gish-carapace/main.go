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

	pluginapi "github.com/blairham/gish/pkg/pluginapi/v1"
	pluginsdk "github.com/blairham/gish/pkg/pluginsdk/v1"
)

type info struct {
	pluginapi.UnimplementedPluginInfoServer
	caps []pluginapi.Capability
}

func (i info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:         "gish-carapace",
		Version:      "0.1.0",
		Capabilities: i.caps,
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

// newPlugin wires the services this binary serves. main and the tests build
// it through here, so a capability cannot be claimed in Describe without the
// service behind it actually being registered.
func newPlugin() pluginsdk.Plugin {
	p := pluginsdk.Plugin{Completion: completion{bridge: newBridge()}}
	p.Info = info{caps: pluginsdk.Capabilities(p)}
	return p
}

func main() { pluginsdk.Serve(newPlugin()) }
