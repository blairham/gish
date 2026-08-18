package main

import (
	"context"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/promptengine"
	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
)

// isolate points the native config lookup at an empty directory, so the
// tests describe the plugin rather than the developer's own prompt.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestDescribeClaimsTheThemeCapability(t *testing.T) {
	// Through newPlugin, not a bare info{}: the claim under test is what the
	// shipped binary announces, and Describe now reports the services that
	// were actually registered rather than a hand-kept list beside them.
	got, err := newPlugin().Info.Describe(context.Background(), &pluginapi.DescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetName() != "koi-p10k" {
		t.Errorf("name = %q", got.GetName())
	}
	caps := got.GetCapabilities()
	if len(caps) != 1 || caps[0] != pluginapi.Capability_CAPABILITY_THEME {
		t.Errorf("capabilities = %v, want just CAPABILITY_THEME", caps)
	}
}

func TestThemesCoverEveryPreset(t *testing.T) {
	got, err := theme{}.Themes(context.Background(), &pluginapi.ThemesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetThemes()) != len(promptengine.Presets()) {
		t.Fatalf("advertised %d themes, have %d presets", len(got.GetThemes()), len(promptengine.Presets()))
	}
	for _, th := range got.GetThemes() {
		// Built-in names are reserved by the host and would be dropped
		// silently, so every advertised name must be prefixed.
		if !strings.HasPrefix(th.GetName(), "p10k-") {
			t.Errorf("theme %q would collide with a reserved name", th.GetName())
		}
		if th.GetBudgetMs() == 0 {
			t.Errorf("theme %q declares no budget", th.GetName())
		}
	}
}

func sampleRequest(name string) *pluginapi.RenderPromptRequest {
	return &pluginapi.RenderPromptRequest{
		Theme: name,
		Context: &pluginapi.PromptContext{
			Cwd:          "/fixture/you/dev/koi",
			Home:         "/fixture/you",
			Username:     "you",
			Hostname:     "host",
			LastExitCode: 1,
			DurationMs:   4000,
			Width:        100,
			Color:        true,
		},
	}
}

func TestRenderPromptServesEachPreset(t *testing.T) {
	isolate(t)
	for _, preset := range promptengine.Presets() {
		t.Run(preset, func(t *testing.T) {
			got, err := theme{}.RenderPrompt(context.Background(), sampleRequest("p10k-"+preset))
			if err != nil {
				t.Fatal(err)
			}
			if got.GetPrompt() == "" {
				t.Fatal("empty prompt: the host would fall back to the built-in theme")
			}
			if !strings.Contains(got.GetPrompt(), "koi") {
				t.Errorf("prompt does not show the directory: %q", got.GetPrompt())
			}
			if got.GetContPrompt() == "" {
				t.Error("no continuation prompt")
			}
		})
	}
}

func TestRenderPromptMatchesTheInProcessEngine(t *testing.T) {
	isolate(t)
	// The whole point of sharing internal/promptengine is that the two front
	// doors cannot drift. Same inputs, same bytes.
	req := sampleRequest("p10k-lean")
	got, err := theme{}.RenderPrompt(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := promptengine.Render(promptengine.Preset("lean"), promptContext(req.GetContext()))
	if got.GetPrompt() != want.Prompt {
		t.Errorf("plugin and in-process prompts differ:\n plugin: %q\n direct: %q", got.GetPrompt(), want.Prompt)
	}
	if got.GetRprompt() != want.RPrompt {
		t.Errorf("rprompts differ:\n plugin: %q\n direct: %q", got.GetRprompt(), want.RPrompt)
	}
}

func TestRenderPromptUnknownThemeStillAnswers(t *testing.T) {
	isolate(t)
	// The host only asks for names we advertised, but a wrong answer
	// here would blank the prompt. Degrade to the default instead.
	got, err := theme{}.RenderPrompt(context.Background(), sampleRequest("p10k-nonsense"))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetPrompt() == "" {
		t.Error("unknown theme produced no prompt")
	}
}

func TestRenderPromptWithoutContext(t *testing.T) {
	isolate(t)
	// A malformed request must not take the plugin down: a panic here
	// costs the user their prompt and triggers the host's crash healing.
	got, err := theme{}.RenderPrompt(context.Background(), &pluginapi.RenderPromptRequest{Theme: "p10k-lean"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetPrompt() == "" {
		t.Error("empty context produced no prompt at all")
	}
}
