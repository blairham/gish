// Command gish-p10k serves the native powerlevel10k presets over the
// tier-2 ThemeProvider contract (#30) — the first real implementation of
// that service, and the demonstration that a whole prompt can live
// behind the plugin boundary rather than only a segment of one.
//
// gish itself does not need this: GISH_THEME=p10k renders in-process,
// because the shell's own prompt should not pay a round trip to draw
// itself. What this binary is for is everything else — another shell
// speaking the same protocol, a theme distributed and updated
// independently of the gish release it runs against, and a working
// example for anyone writing a ThemeProvider of their own.
//
// The engine is shared with the in-process path (internal/promptengine), so the
// two cannot drift: one implementation, two front doors.
package main

import (
	"context"
	"os"
	"time"

	"github.com/hashicorp/go-plugin"

	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/internal/promptengine"
	"github.com/blairham/gish/pkg/pluginapi"
)

// renderBudget is what this plugin declares to the host. The host
// derives its deadline from it and serves the previous prompt on a miss,
// so the number is a promise about the common case rather than a
// guarantee — but the engine renders in tens of microseconds, so the
// margin here is enormous and deliberate: it covers a cold page cache
// and a loaded machine without ever making the shell wait on us.
const renderBudget = 50

type info struct {
	pluginapi.UnimplementedPluginInfoServer
}

func (info) Describe(context.Context, *pluginapi.DescribeRequest) (*pluginapi.DescribeResponse, error) {
	return &pluginapi.DescribeResponse{
		Name:    "gish-p10k",
		Version: "0.1.0",
		Capabilities: []pluginapi.Capability{
			pluginapi.Capability_CAPABILITY_THEME,
		},
	}, nil
}

type theme struct {
	pluginapi.UnimplementedThemeProviderServer
}

// Themes advertises one theme per preset.
//
// The names are prefixed because the host reserves the built-in names
// and will not let a plugin claim them: "p10k" already means the
// in-process engine. Serving "p10k-rainbow" and friends is the honest
// spelling anyway — it says which implementation you are getting.
func (theme) Themes(context.Context, *pluginapi.ThemesRequest) (*pluginapi.ThemesResponse, error) {
	descriptors := make([]*pluginapi.ThemeDescriptor, 0, len(promptengine.Presets()))
	for _, name := range promptengine.Presets() {
		descriptors = append(descriptors, &pluginapi.ThemeDescriptor{
			Name:        "p10k-" + name,
			Description: "powerlevel10k " + name + " preset, rendered natively",
			BudgetMs:    renderBudget,
		})
	}
	return &pluginapi.ThemesResponse{Themes: descriptors}, nil
}

func (theme) RenderPrompt(
	_ context.Context, req *pluginapi.RenderPromptRequest,
) (*pluginapi.RenderPromptResponse, error) {
	cfg := configFor(req.GetTheme())
	out := promptengine.Render(cfg, promptContext(req.GetContext()))
	return &pluginapi.RenderPromptResponse{
		Prompt:     out.Prompt,
		ContPrompt: out.Cont,
		Rprompt:    out.RPrompt,
	}, nil
}

// configFor resolves a theme name to its configuration. The user's own
// native config layers on top, so a prompt served over the socket is the
// same prompt the in-process path would draw.
func configFor(name string) *promptengine.Config {
	preset := promptengine.Preset(trimThemePrefix(name))
	if preset == nil {
		preset = promptengine.Preset(promptengine.DefaultPreset)
	}
	if file := promptengine.LoadNativeConfig(); file != nil {
		preset = preset.Clone()
		preset.Merge(file)
	}
	return preset
}

func trimThemePrefix(name string) string {
	if len(name) > 5 && name[:5] == "p10k-" {
		return name[5:]
	}
	return name
}

// promptContext converts the wire shape into what a segment reads.
//
// Environment is the notable gap: the host serves plugins an allowlisted
// subset rather than the session's whole environment, and the theme
// service has no per-theme env declaration (prompt *segments* do, via
// SegmentDescriptor.env_keys). So the env-driven segments — virtualenv,
// the cloud contexts — see this process's environment, which is the
// launching shell's at plugin start. Anything set after that is not
// visible here. That is a real difference from the in-process path, and
// the reason the in-process path is the default.
func promptContext(c *pluginapi.PromptContext) *promptengine.Context {
	if c == nil {
		return &promptengine.Context{Getenv: os.Getenv}
	}
	ctx := &promptengine.Context{
		Cwd:      c.GetCwd(),
		Home:     c.GetHome(),
		Username: c.GetUsername(),
		Hostname: c.GetHostname(),
		ExitCode: int(c.GetLastExitCode()),
		Duration: time.Duration(c.GetDurationMs()) * time.Millisecond,
		Jobs:     int(c.GetJobs()),
		Width:    int(c.GetWidth()),
		SSH:      c.GetSsh(),
		Root:     os.Geteuid() == 0,
		Getenv:   os.Getenv,
	}
	ctx.Git = promptengine.HeadStatus(ctx.Cwd)
	return ctx
}

func main() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: pluginhost.Handshake,
		Plugins: map[string]plugin.Plugin{
			"info":       &pluginhost.InfoPlugin{Impl: info{}},
			"theme":      &pluginhost.ThemePlugin{Impl: theme{}},
			"prompt":     &pluginhost.PromptPlugin{},
			"completion": &pluginhost.CompletionPlugin{},
			"history":    &pluginhost.HistoryPlugin{},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
