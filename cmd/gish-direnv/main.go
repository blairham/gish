// Command gish-direnv drives the user's own direnv through the
// EnvProvider contract (#137).
//
// This lands squarely on the #112 native/delegate line: **use real
// direnv, do not reimplement it.**
//
//   - Delegated to direnv: .envrc evaluation and the whole stdlib —
//     `use nix`, `layout python`, `dotenv`, `source_up`, the version
//     helpers. Thousands of lines someone maintains full time, with
//     semantics users already depend on. Reimplementing it would be the
//     `tool install --from` mistake at ten times the size.
//   - Owned by gish: the *cd moment* (no `eval "$(direnv hook bash)"` —
//     the shell already notices directory changes, which is the whole
//     point of #12), the approval UX, and applying and reverting the
//     diff, including the subtree revert.
//
// The mechanism was already the right shape: `direnv export json` emits
// exactly an env diff, which is what EnvProvider carries. So this plugin
// is a translator, not an implementation:
//
//	cd → host asks the plugin → direnv export json → diff → #12 trust → applied
//
// The diff arrives with gish's guarantees intact: nothing applies until
// allowed, deny-listed variables are stripped host-side, and applied
// diffs revert when the shell leaves the subtree.
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
		Name:    "gish-direnv",
		Version: "0.1.0",
		Capabilities: []pluginapi.Capability{
			pluginapi.Capability_CAPABILITY_ENV,
		},
	}, nil
}

type provider struct {
	pluginapi.UnimplementedEnvProviderServer
	bridge *bridge
}

// EnvDiff proposes what the .envrc in scope would set.
//
// The allow state is checked before exporting, and that ordering is the
// whole point. A blocked .envrc does not make `direnv export json` fail
// — it exits 0 and returns only bookkeeping — so exporting first would
// yield an empty diff indistinguishable from "this directory has no
// .envrc", and the user would watch their .envrc do nothing with no
// explanation anywhere.
//
// Instead a blocked directory returns an empty diff *with* for_dir set,
// which tells the host there is something here awaiting approval. gish's
// `trust allow` then runs `direnv allow` and re-asks: one gesture, both
// trust models satisfied.
func (p provider) EnvDiff(ctx context.Context, req *pluginapi.EnvDiffRequest) (*pluginapi.EnvDiffResponse, error) {
	cwd := req.GetCwd()
	if cwd == "" {
		return &pluginapi.EnvDiffResponse{}, nil
	}
	env := req.GetEnv()

	state, rcPath, err := p.bridge.state(ctx, cwd, env)
	if err != nil {
		// No direnv, or it could not answer in the budget. An empty
		// response means "nothing for this cwd" — the shell carries on.
		// A missing direnv is a normal state, not a failure.
		return &pluginapi.EnvDiffResponse{}, nil //nolint:nilerr // degradation is the contract
	}
	switch state {
	case stateNoRC, stateDenied:
		// Denied is the user's own explicit `direnv deny`. Honoring it
		// silently is correct: re-proposing what someone has refused
		// would be nagging, and gish is not the authority here.
		return &pluginapi.EnvDiffResponse{}, nil
	case stateBlocked:
		// Something is here, awaiting approval. for_dir with an empty
		// set is the honest encoding of that.
		return &pluginapi.EnvDiffResponse{ForDir: rcDir(rcPath, cwd)}, nil
	}

	set, err := p.bridge.export(ctx, cwd, env)
	if err != nil {
		return &pluginapi.EnvDiffResponse{}, nil //nolint:nilerr // degradation is the contract
	}
	return &pluginapi.EnvDiffResponse{ForDir: rcDir(rcPath, cwd), Set: set}, nil
}

// Allow is the second half of the one-gesture trust flow: the user
// approved in gish, so direnv is told too.
//
// One gesture is only safe because both systems key on *content*.
// Editing the .envrc re-blocks direnv and changes gish's diff hash, so
// the two re-prompt together rather than drifting into a state where
// gish thinks a directory is trusted and direnv silently exports
// nothing. That was the open question on #137 and it was tested, not
// assumed.
func (p provider) Allow(ctx context.Context, req *pluginapi.AllowRequest) (*pluginapi.AllowResponse, error) {
	dir := req.GetForDir()
	if dir == "" {
		return &pluginapi.AllowResponse{}, nil
	}
	if err := p.bridge.allow(ctx, dir, req.GetEnv()); err != nil {
		// The host still applies the diff it showed the user — gish's
		// own record is authoritative for gish — but it needs to say
		// that direnv did not record it, or the next shell re-prompts
		// for no visible reason.
		return &pluginapi.AllowResponse{Recorded: false, Detail: err.Error()}, nil
	}
	return &pluginapi.AllowResponse{Recorded: true}, nil
}

func main() {
	b := &bridge{}
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: pluginhost.Handshake,
		Plugins: map[string]plugin.Plugin{
			"info":       &pluginhost.InfoPlugin{Impl: info{}},
			"env":        &pluginhost.EnvPlugin{Impl: provider{bridge: b}},
			"completion": &pluginhost.CompletionPlugin{},
			"prompt":     &pluginhost.PromptPlugin{},
			"history":    &pluginhost.HistoryPlugin{},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
