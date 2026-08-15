package repl

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/blairham/gish/pkg/pluginapi"
)

// fakeThemeClient implements pluginapi.ThemeProviderClient without a
// process: an optional delay exercises the budget, err forces misses.
type fakeThemeClient struct {
	delay time.Duration
	resp  *pluginapi.RenderPromptResponse
	err   error
}

func (f *fakeThemeClient) Themes(
	context.Context, *pluginapi.ThemesRequest, ...grpc.CallOption,
) (*pluginapi.ThemesResponse, error) {
	return &pluginapi.ThemesResponse{}, nil
}

func (f *fakeThemeClient) RenderPrompt(
	ctx context.Context, _ *pluginapi.RenderPromptRequest, _ ...grpc.CallOption,
) (*pluginapi.RenderPromptResponse, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.resp, f.err
}

func testThemeRenderer(client pluginapi.ThemeProviderClient, budget time.Duration) *themeRenderer {
	warmed := make(chan struct{})
	close(warmed)
	return &themeRenderer{
		warmed: warmed,
		themes: map[string]themeEntry{"neo": {client: client, budget: budget}},
		last:   map[string]promptSet{},
	}
}

func TestThemeRendererRendersAndCaches(t *testing.T) {
	t.Parallel()

	fake := &fakeThemeClient{resp: &pluginapi.RenderPromptResponse{
		Prompt: "neo> ", ContPrompt: "n| ", Rprompt: "R",
	}}
	r := testThemeRenderer(fake, 50*time.Millisecond)

	set, ok := r.render(context.Background(), "neo", themeInfo())
	if !ok || set.prompt != "neo> " || set.cont != "n| " || set.rprompt != "R" {
		t.Fatalf("render = %+v, %v", set, ok)
	}
	if got := r.last["neo"]; got != set {
		t.Errorf("stale cache not primed: %+v", got)
	}
}

func TestThemeRendererBudgetMissServesStale(t *testing.T) {
	t.Parallel()

	fake := &fakeThemeClient{
		delay: 500 * time.Millisecond,
		resp:  &pluginapi.RenderPromptResponse{Prompt: "late> "},
	}
	r := testThemeRenderer(fake, 20*time.Millisecond)
	r.last["neo"] = promptSet{prompt: "stale> ", cont: "s| "}

	set, ok := r.render(context.Background(), "neo", themeInfo())
	if !ok || set.prompt != "stale> " {
		t.Fatalf("budget miss should serve stale, got %+v, %v", set, ok)
	}
}

func TestThemeRendererMissWithoutStaleFails(t *testing.T) {
	t.Parallel()

	fake := &fakeThemeClient{err: context.DeadlineExceeded}
	r := testThemeRenderer(fake, 20*time.Millisecond)
	if _, ok := r.render(context.Background(), "neo", themeInfo()); ok {
		t.Error("miss with no stale value should report false")
	}
	// An empty prompt is a miss too: a theme must produce a prompt.
	empty := &fakeThemeClient{resp: &pluginapi.RenderPromptResponse{}}
	r = testThemeRenderer(empty, 20*time.Millisecond)
	if _, ok := r.render(context.Background(), "neo", themeInfo()); ok {
		t.Error("empty prompt should report false")
	}
}

func TestThemeRendererUnknownName(t *testing.T) {
	t.Parallel()

	r := testThemeRenderer(&fakeThemeClient{}, 20*time.Millisecond)
	if _, ok := r.render(context.Background(), "nonexistent", themeInfo()); ok {
		t.Error("unknown theme name should report false")
	}
}

// fakePluginThemes stubs the resolution seam.
type fakePluginThemes struct {
	set promptSet
	ok  bool
}

func (f *fakePluginThemes) render(context.Context, string, promptInfo) (promptSet, bool) {
	return f.set, f.ok
}

func TestPromptStringsResolvesPluginTheme(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	t.Cleanup(func() { themePlugins = nil })

	runner := newTestRunner(t)
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=neo`)); err != nil {
		t.Fatal(err)
	}

	// A plugin claims the name: its full prompt set wins.
	themePlugins = &fakePluginThemes{set: promptSet{prompt: "neo> ", cont: "n| ", rprompt: "R"}, ok: true}
	p, cp, rp := promptStrings(runner, themeInfo())
	if p != "neo> " || cp != "n| " || rp != "R" {
		t.Errorf("plugin theme not resolved: %q %q %q", p, cp, rp)
	}

	// Nothing serves the name: the native theme renders, not naked.
	themePlugins = &fakePluginThemes{}
	if p, _, _ = promptStrings(runner, themeInfo()); !strings.Contains(p, "❯") {
		t.Errorf("unserved theme name should fall back to the native theme: %q", p)
	}

	// Built-in names cannot be hijacked by a plugin.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_THEME=plain`)); err != nil {
		t.Fatal(err)
	}
	themePlugins = &fakePluginThemes{set: promptSet{prompt: "hijacked> "}, ok: true}
	if p, _, _ = promptStrings(runner, themeInfo()); p != "blair@mba gish % " {
		t.Errorf("built-in name resolved through a plugin: %q", p)
	}
}
