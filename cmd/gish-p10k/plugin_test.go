package main_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

// A real round trip: build the plugin, let the host discover and launch
// it, and ask it for a prompt over gRPC.
//
// This is the first end-to-end exercise of the ThemeProvider service
// (#30). The service, the host wiring and the reserved-name rules were
// all written before anything implemented them, so this is what shows
// the contract actually closes: discovery sees the capability, the host
// launches on demand, and a rendered prompt comes back over the socket.
func hostWithPlugin(t *testing.T) *pluginhost.Host {
	t.Helper()
	dir := t.TempDir()
	name := "gish-p10k"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	build := exec.Command("go", "build", "-o", filepath.Join(dir, name), ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the plugin: %v\n%s", err, out)
	}

	h := pluginhost.NewHost(dir, pluginhost.WithBackoff(10*time.Millisecond))
	if err := h.Discover(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func TestPluginRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a plugin binary")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	h := hostWithPlugin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	providers := h.ThemeProviders(ctx)
	if len(providers) != 1 {
		t.Fatalf("host found %d theme providers, want 1", len(providers))
	}
	client := providers[0].Client

	themes, err := client.Themes(ctx, &pluginapi.ThemesRequest{})
	if err != nil {
		t.Fatalf("Themes: %v", err)
	}
	if len(themes.GetThemes()) == 0 {
		t.Fatal("plugin advertised no themes")
	}

	got, err := client.RenderPrompt(ctx, &pluginapi.RenderPromptRequest{
		Theme: themes.GetThemes()[0].GetName(),
		Context: &pluginapi.PromptContext{
			Cwd: "/fixture/you/dev/gish", Home: "/fixture/you",
			Username: "you", Hostname: "host", Width: 100, Color: true,
		},
	})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if !strings.Contains(got.GetPrompt(), "gish") {
		t.Errorf("prompt came back over the socket without the directory: %q", got.GetPrompt())
	}
}

// TestPluginRendersInsideItsBudget checks the promise the plugin makes
// to the host. A miss is survivable — the host serves the previous
// prompt — but a theme that routinely misses its own declared budget is
// one that shows you a stale prompt, which is the failure mode this
// whole contract exists to bound.
func TestPluginRendersInsideItsBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a plugin binary")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	h := hostWithPlugin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	providers := h.ThemeProviders(ctx)
	if len(providers) == 0 {
		t.Fatal("no theme provider")
	}
	client := providers[0].Client
	req := &pluginapi.RenderPromptRequest{
		Theme: "p10k-lean",
		Context: &pluginapi.PromptContext{
			Cwd: "/fixture/you/dev/gish", Home: "/fixture/you", Width: 100, Color: true,
		},
	}
	// Warm the connection first: the first call includes the launch.
	if _, err := client.RenderPrompt(ctx, req); err != nil {
		t.Fatal(err)
	}

	best := time.Hour
	for range 10 {
		start := time.Now()
		if _, err := client.RenderPrompt(ctx, req); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d < best {
			best = d
		}
	}
	budget := time.Duration(renderBudgetMs) * time.Millisecond
	t.Logf("best round trip: %v (declared budget %v)", best.Round(time.Microsecond), budget)
	if best > budget {
		t.Errorf("best round trip %v misses the plugin's own declared budget %v", best, budget)
	}
}

// renderBudgetMs mirrors the constant the plugin declares. It is
// duplicated rather than exported because this is an external test
// package, and the value is part of what is being asserted.
const renderBudgetMs = 50
