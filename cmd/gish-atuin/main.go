// Command gish-atuin bridges the user's own atuin installation into
// gish as a tier-2 HistoryBackend (#97).
//
// atuin (~31k stars) exists because shell history is a flat local file,
// and its threads are full of "shells should have this built in". That
// makes it the highest-signal proof of the HistoryBackend contract: the
// proto was designed for exactly this shape, and if a real sync engine
// drops in behind it without the shell learning anything about atuin,
// the contract is right.
//
// This is a **bridge, not a reimplementation**. atuin's community-vetted
// posture — opt-in, self-hostable, end-to-end encrypted — is the point,
// and gish reimplementing sync would inherit none of it. The user's own
// atuin binary, config, server, and keys stay in charge.
//
// Two invariants hold no matter what atuin does:
//
//   - The local JSONL file stays authoritative. Zero plugins installed
//     is a fully working history; this only ever adds.
//   - Scrubbed commands never arrive here. The shell's store is the gate
//     (#10): a secret-bearing command is never recorded, so it is never
//     fanned out. A backend cannot leak what it is never given.
package main

import (
	"context"
	"errors"

	pluginapi "github.com/blairham/gish/pkg/pluginapi/v1"
	pluginsdk "github.com/blairham/gish/pkg/pluginsdk/v1"
)

// errNoAtuin means the user has no atuin. It is a normal state, not a
// failure: Append declines and Search returns nothing, so a gish with
// this plugin installed but atuin uninstalled behaves exactly like a
// gish without the plugin.
var errNoAtuin = errors.New("atuin is not installed")

type info struct {
	pluginapi.UnimplementedPluginInfoServer
	caps []pluginapi.Capability
}

func (i info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:         "gish-atuin",
		Version:      "0.1.0",
		Capabilities: i.caps,
	}, nil
}

type backend struct {
	pluginapi.UnimplementedHistoryBackendServer
	bridge *bridge
}

// Append mirrors one executed command into atuin.
//
// gish hands over a *finished* command; atuin models a command as start
// then end. So this does both, back to back. One consequence worth
// stating: atuin stamps the record when `history start` runs, so a
// mirrored entry's timestamp is when gish told atuin about it, not when
// the command actually began — atuin's CLI has no way to set the
// timestamp. The skew is the command's own duration plus fan-out
// latency, which is invisible for interactive work and visible for a
// command that ran for an hour. Search results carry atuin's timestamp,
// so the two agree with each other.
//
// stored=false governs only atuin's copy; the shell's local history is
// unaffected either way.
func (b backend) Append(ctx context.Context, req *pluginapi.AppendRequest) (*pluginapi.AppendResponse, error) {
	e := req.GetEntry()
	if e == nil || e.GetCommand() == "" {
		return &pluginapi.AppendResponse{Stored: false}, nil
	}
	id, err := b.bridge.start(ctx, e.GetCommand())
	if err != nil {
		// Not an RPC error: the host fans out fire-and-forget, and an
		// error here would be noise nobody can act on mid-session.
		// doctor is where a broken atuin should surface.
		return &pluginapi.AppendResponse{Stored: false}, nil //nolint:nilerr // degradation is the contract
	}
	if err := b.bridge.end(ctx, id, e.GetExitCode(), e.GetDurationMs()); err != nil {
		// The record exists but has no duration or status. Still better
		// than nothing, and atuin treats a zero duration as "unfinished"
		// rather than as a lie.
		return &pluginapi.AppendResponse{Stored: true}, nil //nolint:nilerr // partial success is still stored
	}
	return &pluginapi.AppendResponse{Stored: true}, nil
}

// Search serves ctrl-r from atuin's database — the cross-machine half,
// which is the reason anyone installs atuin in the first place.
//
// One batch, marked final. Streaming exists in the proto for backends
// that can produce results incrementally; shelling out to a CLI cannot,
// and pretending otherwise would add latency without adding results.
func (b backend) Search(req *pluginapi.SearchRequest, stream pluginapi.HistoryBackend_SearchServer) error {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	results, err := b.bridge.search(stream.Context(), req.GetQuery(), req.GetCwd(), limit, req.GetPrefixOnly())
	if err != nil {
		// An empty final batch, not an error: the host merges what
		// backends return and falls back to local history. A user whose
		// atuin is missing or misconfigured gets gish's own history, not
		// a broken ctrl-r.
		return stream.Send(&pluginapi.SearchBatch{Final: true})
	}
	batch := &pluginapi.SearchBatch{Final: true}
	for _, r := range results {
		batch.Entries = append(batch.Entries, &pluginapi.HistoryEntry{
			Command:       r.Command,
			Cwd:           r.Cwd,
			ExitCode:      r.ExitCode,
			StartedUnixMs: r.StartedUnixMs,
		})
	}
	return stream.Send(batch)
}

// newPlugin wires the services this binary serves. main and the tests build
// it through here, so a capability cannot be claimed in Describe without the
// service behind it actually being registered.
func newPlugin() pluginsdk.Plugin {
	p := pluginsdk.Plugin{History: backend{bridge: &bridge{}}}
	p.Info = info{caps: pluginsdk.Capabilities(p)}
	return p
}

func main() { pluginsdk.Serve(newPlugin()) }
