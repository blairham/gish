// Package pluginsdk is the front door for writing a gish tier-2 plugin.
//
// A plugin is an ordinary executable that gish launches and keeps resident,
// speaking gRPC over hashicorp/go-plugin. Implement one or more of the
// services in pkg/pluginapi, hand them to Serve, and drop the binary in
// $XDG_DATA_HOME/gish/plugins:
//
//	func main() {
//		pluginsdk.Serve(pluginsdk.Plugin{
//			Info:   info{},                  // required
//			Prompt: prompt{cache: cache},    // whatever else you implement
//		})
//	}
//
// This package is the *serve* side of the contract and nothing else. How gish
// discovers plugins, when it launches them, and how it heals a crashed one are
// the host's business (internal/pluginhost) — deliberately unpublished, so the
// lifecycle stays free to change while the contract does not.
//
// The contract itself is frozen: proto/gish/plugin/v1 is additive-only and
// internal/pluginhost/abi_test.go fails CI on any change that would break a
// compiled plugin (#168). A binary built against v1 keeps working across gish
// releases without a rebuild — which is the promise this package exists to
// make usable from outside the repository (#188).
//
// Two rules a plugin author inherits, both enforced by the host rather than by
// convention:
//
//   - Every host→plugin call carries a deadline. A slow answer is dropped or
//     served stale; it never blocks a keystroke. Return something fast and
//     imperfect rather than something slow and exact.
//   - A plugin never holds an exec channel. Commands a plugin proposes land in
//     the editor buffer or behind an approval gate; the shell owns execution.
package pluginsdk

import (
	"context"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	pluginapi "github.com/blairham/gish/pkg/pluginapi/v1"
)

// Handshake gates plugin startup: a binary that doesn't present the magic
// cookie is not treated as a koi plugin. ProtocolVersion tracks the proto
// package version (koi.plugin.v1 == 1); bumping it is a breaking change and
// requires a v2 proto package.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "KOI_PLUGIN",
	MagicCookieValue: "koi.plugin.v1",
}

// Service names under which capabilities are dispensed. Both sides of the
// go-plugin connection key on these strings, so they are part of the ABI.
const (
	ServiceInfo       = "info"
	ServiceCompletion = "completion"
	ServicePrompt     = "prompt"
	ServiceHistory    = "history"
	ServiceCommand    = "command"
	ServiceTheme      = "theme"
	ServiceEnv        = "env"
	ServiceAI         = "ai"
)

// Plugin is what a binary serves: the mandatory PluginInfo service plus
// whichever capabilities it implements. A nil field is a capability this
// plugin does not have, and Serve registers no service for it — the absent
// field is the whole declaration.
//
// That is deliberate. Registering a service with no implementation behind it
// leaves a door the host can open onto a nil pointer, and the only thing
// keeping it shut is the host asking Describe first. An unspellable bug beats
// a well-behaved host.
type Plugin struct {
	// Info is required: the host calls Describe immediately after the
	// handshake to learn the plugin's name and capabilities.
	Info pluginapi.PluginInfoServer

	Completion pluginapi.CompletionProviderServer
	Prompt     pluginapi.PromptSegmentProviderServer
	History    pluginapi.HistoryBackendServer
	Command    pluginapi.CommandProviderServer
	Theme      pluginapi.ThemeProviderServer
	Env        pluginapi.EnvProviderServer
	AI         pluginapi.AIProviderServer
}

// Capabilities reports the capabilities implied by p's non-nil fields, in
// enum order. Return it from Describe rather than hand-listing:
//
//	func (i info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
//		return &pluginapi.DescribeResponse{
//			Name:         "gish-example",
//			Version:      version,
//			Capabilities: pluginsdk.Capabilities(i.plugin),
//		}, nil
//	}
//
// The host gates every dispatch on Describe, so a capability the plugin
// implements but forgets to announce is silently dead — never called, no
// error, nothing in the logs. Deriving the list from the same struct that
// registers the services is what keeps the two from drifting apart.
//
// CAPABILITY_EVENTS has no field: ShellEvents (#83) is defined in the protos
// and allocated a capability, but the host does not serve it yet.
func Capabilities(p Plugin) []pluginapi.Capability {
	var caps []pluginapi.Capability
	if p.Completion != nil {
		caps = append(caps, pluginapi.Capability_CAPABILITY_COMPLETION)
	}
	if p.Prompt != nil {
		caps = append(caps, pluginapi.Capability_CAPABILITY_PROMPT_SEGMENT)
	}
	if p.History != nil {
		caps = append(caps, pluginapi.Capability_CAPABILITY_HISTORY)
	}
	if p.Command != nil {
		caps = append(caps, pluginapi.Capability_CAPABILITY_COMMAND)
	}
	if p.Theme != nil {
		caps = append(caps, pluginapi.Capability_CAPABILITY_THEME)
	}
	if p.Env != nil {
		caps = append(caps, pluginapi.Capability_CAPABILITY_ENV)
	}
	if p.AI != nil {
		caps = append(caps, pluginapi.Capability_CAPABILITY_AI)
	}
	return caps
}

// Serve runs p as a gish plugin. It does not return: go-plugin owns the
// process from here, and the host's shutdown is what ends it.
func Serve(p Plugin) {
	plugin.Serve(ServeConfig(p))
}

// ServeConfig builds the go-plugin server configuration for p, for authors who
// need to adjust it (a custom logger, a TLS provider) before serving. Most
// plugins want Serve.
func ServeConfig(p Plugin) *plugin.ServeConfig {
	return &plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         ServeMap(p),
		GRPCServer:      plugin.DefaultGRPCServer,
	}
}

// ServeMap is the go-plugin dispenser map for p's non-nil services.
func ServeMap(p Plugin) map[string]plugin.Plugin {
	m := map[string]plugin.Plugin{}
	if p.Info != nil {
		m[ServiceInfo] = &InfoPlugin{Impl: p.Info}
	}
	if p.Completion != nil {
		m[ServiceCompletion] = &CompletionPlugin{Impl: p.Completion}
	}
	if p.Prompt != nil {
		m[ServicePrompt] = &PromptPlugin{Impl: p.Prompt}
	}
	if p.History != nil {
		m[ServiceHistory] = &HistoryPlugin{Impl: p.History}
	}
	if p.Command != nil {
		m[ServiceCommand] = &CommandPlugin{Impl: p.Command}
	}
	if p.Theme != nil {
		m[ServiceTheme] = &ThemePlugin{Impl: p.Theme}
	}
	if p.Env != nil {
		m[ServiceEnv] = &EnvPlugin{Impl: p.Env}
	}
	if p.AI != nil {
		m[ServiceAI] = &AIPlugin{Impl: p.AI}
	}
	return m
}

// HostMap names every capability service a plugin process may serve. The host
// side of the connection needs all of them, since it learns from Describe
// which to dispense; the plugin side registers only what it implements (see
// ServeMap). Plugin authors do not need this.
func HostMap() map[string]plugin.Plugin {
	return map[string]plugin.Plugin{
		ServiceInfo:       &InfoPlugin{},
		ServiceCompletion: &CompletionPlugin{},
		ServicePrompt:     &PromptPlugin{},
		ServiceHistory:    &HistoryPlugin{},
		ServiceCommand:    &CommandPlugin{},
		ServiceTheme:      &ThemePlugin{},
		ServiceEnv:        &EnvPlugin{},
		ServiceAI:         &AIPlugin{},
	}
}

// InfoPlugin dispenses the mandatory PluginInfo service.
type InfoPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	// Impl is set on the plugin side; the host side leaves it nil.
	Impl pluginapi.PluginInfoServer
}

func (p *InfoPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pluginapi.RegisterPluginInfoServer(s, p.Impl)
	return nil
}

func (p *InfoPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginapi.NewPluginInfoClient(c), nil
}

// CompletionPlugin dispenses CompletionProvider.
type CompletionPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl pluginapi.CompletionProviderServer
}

func (p *CompletionPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pluginapi.RegisterCompletionProviderServer(s, p.Impl)
	return nil
}

func (p *CompletionPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginapi.NewCompletionProviderClient(c), nil
}

// PromptPlugin dispenses PromptSegmentProvider.
type PromptPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl pluginapi.PromptSegmentProviderServer
}

func (p *PromptPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pluginapi.RegisterPromptSegmentProviderServer(s, p.Impl)
	return nil
}

func (p *PromptPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginapi.NewPromptSegmentProviderClient(c), nil
}

// HistoryPlugin dispenses HistoryBackend.
type HistoryPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl pluginapi.HistoryBackendServer
}

func (p *HistoryPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pluginapi.RegisterHistoryBackendServer(s, p.Impl)
	return nil
}

func (p *HistoryPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginapi.NewHistoryBackendClient(c), nil
}

// ThemePlugin dispenses ThemeProvider (#30).
type ThemePlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl pluginapi.ThemeProviderServer
}

func (p *ThemePlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pluginapi.RegisterThemeProviderServer(s, p.Impl)
	return nil
}

func (p *ThemePlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginapi.NewThemeProviderClient(c), nil
}

// EnvPlugin dispenses EnvProvider (#12).
type EnvPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl pluginapi.EnvProviderServer
}

func (p *EnvPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pluginapi.RegisterEnvProviderServer(s, p.Impl)
	return nil
}

func (p *EnvPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginapi.NewEnvProviderClient(c), nil
}

// AIPlugin dispenses AIProvider (#20).
type AIPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl pluginapi.AIProviderServer
}

func (p *AIPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pluginapi.RegisterAIProviderServer(s, p.Impl)
	return nil
}

func (p *AIPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginapi.NewAIProviderClient(c), nil
}

// CommandPlugin dispenses CommandProvider (#11).
type CommandPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl pluginapi.CommandProviderServer
}

func (p *CommandPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pluginapi.RegisterCommandProviderServer(s, p.Impl)
	return nil
}

func (p *CommandPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginapi.NewCommandProviderClient(c), nil
}
