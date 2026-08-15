// Package pluginhost manages tier-2 native plugins: resident subprocesses
// speaking gRPC via hashicorp/go-plugin (the same architecture as the
// cloudctl/understudy/chaos-lab tools).
//
// Lifecycle: plugins are launched lazily on first use, kept resident for
// the session, and every host→plugin call carries a deadline so a slow
// plugin degrades (stale prompt segment, missing completions) instead of
// blocking a keystroke. Discovery, configuration, and the actual dispatch
// paths land with the line editor.
package pluginhost

import (
	"context"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/blairham/gish/pkg/pluginapi"
)

// Handshake gates plugin startup: a binary that doesn't present the magic
// cookie is not treated as a gish plugin. ProtocolVersion tracks the
// proto package version (gish.plugin.v1 == 1); bumping it is a breaking
// change and requires a v2 proto package.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "GISH_PLUGIN",
	MagicCookieValue: "gish.plugin.v1",
}

// PluginMap names the capability services a plugin process may serve.
// Every plugin also serves PluginInfo (see common.proto); the host calls
// Describe after the handshake to learn which of these to dispense.
var PluginMap = map[string]plugin.Plugin{
	"info":       &InfoPlugin{},
	"completion": &CompletionPlugin{},
	"prompt":     &PromptPlugin{},
	"history":    &HistoryPlugin{},
	"command":    &CommandPlugin{},
	"theme":      &ThemePlugin{},
	"env":        &EnvPlugin{},
	"ai":         &AIPlugin{},
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
