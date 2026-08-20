// Command koi-dotenv proposes plain .env files through the EnvProvider
// contract (#475).
//
// It is the second real EnvProvider, and that is most of why it exists:
// koi-direnv (#137) proved the contract against a tool with its own
// trust model, and this plugin proves the #12 flow generalizes — no
// wrapped binary, no second approval, no Allow RPC. It also covers the
// far larger population that has .env files but no direnv.
//
// The posture is **parse only — never execute, never expand**:
//
//   - A .env file is data. There is no subprocess anywhere in this
//     plugin, and $VAR stays the literal string "$VAR" — interpolation
//     would need a source environment and rules about it, which is a
//     shell's job, not a file parser's. Anyone who wants expansion or
//     direnv's `dotenv` stdlib helper uses koi-direnv.
//   - Every value travels verbatim to the host, where the #12 trust
//     prompt shows all of them before anything applies. Deny-listed
//     names are stripped host-side, an edited file re-prompts (new diff
//     hash), and the diff reverts when the shell leaves the subtree.
//
// Discovery walks up from the cwd to the nearest .env — direnv's own
// scope rule — and for_dir is the directory that file lives in, so one
// approval covers the whole subtree. Unlike direnv, nothing here ever
// resolves symlinks, so for_dir is already in the caller's namespace
// and needs no mapping back (the rcDir problem koi-direnv has to solve
// does not exist).
//
// Allow is deliberately not implemented: .env has no second trust model
// to satisfy, which is exactly the case the optional RPC's contract
// describes — the plugin answers unimplemented and the host carries on.
package main

import (
	"context"
	"path/filepath"

	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
	pluginsdk "github.com/blairham/koi-shell/pkg/pluginsdk/v1"
)

type info struct {
	pluginapi.UnimplementedPluginInfoServer
	caps []pluginapi.Capability
}

func (i info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:         "koi-dotenv",
		Version:      "0.1.0",
		Capabilities: i.caps,
	}, nil
}

type provider struct {
	pluginapi.UnimplementedEnvProviderServer
}

// EnvDiff proposes the nearest .env's variables for the trust flow.
//
// Everything that can be empty is answered with an empty response, not
// an error: no .env anywhere, an unreadable or oversized file, a file
// of nothing but comments. Unlike koi-direnv there is no blocked state
// to surface — a .env awaits nothing but koi's own `trust allow`, and
// the host derives "pending" from the proposal itself.
func (provider) EnvDiff(_ context.Context, req *pluginapi.EnvDiffRequest) (*pluginapi.EnvDiffResponse, error) {
	cwd := req.GetCwd()
	if cwd == "" {
		return &pluginapi.EnvDiffResponse{}, nil
	}
	path := findDotenv(cwd)
	if path == "" {
		return &pluginapi.EnvDiffResponse{}, nil
	}
	set, err := load(path)
	if err != nil || len(set) == 0 {
		return &pluginapi.EnvDiffResponse{}, nil //nolint:nilerr // degradation is the contract
	}
	return &pluginapi.EnvDiffResponse{ForDir: filepath.Dir(path), Set: set}, nil
}

// newPlugin wires the services this binary serves. main and the tests build
// it through here, so a capability cannot be claimed in Describe without the
// service behind it actually being registered.
func newPlugin() pluginsdk.Plugin {
	p := pluginsdk.Plugin{Env: provider{}}
	p.Info = info{caps: pluginsdk.Capabilities(p)}
	return p
}

func main() { pluginsdk.Serve(newPlugin()) }
