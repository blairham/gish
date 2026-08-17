package promptengine

import (
	"slices"
	"strings"
	"testing"
)

// galleryPresets are the looks taken from upstreams other than
// powerlevel10k (#186).
var galleryPresets = []string{"pastel-powerline", "tokyo-night", "agnoster"}

func TestGalleryPresetsAreRegistered(t *testing.T) {
	for _, name := range galleryPresets {
		if Preset(name) == nil {
			t.Errorf("preset %q is not registered", name)
		}
		if !slices.Contains(Presets(), name) {
			t.Errorf("preset %q is missing from Presets()", name)
		}
	}
}

// Every element a preset names must be a segment that exists. A typo
// here renders as nothing at all, which looks like a broken preset
// rather than a broken string.
func TestEveryPresetElementIsImplemented(t *testing.T) {
	for _, name := range Presets() {
		cfg := Preset(name)
		for _, side := range []string{"LEFT_PROMPT_ELEMENTS", "RIGHT_PROMPT_ELEMENTS"} {
			for _, e := range cfg.List(side) {
				if !Known(e) {
					t.Errorf("preset %q lists %q in %s, which is not a segment", name, e, side)
				}
			}
		}
	}
}

func TestGalleryPresetsRender(t *testing.T) {
	for _, name := range galleryPresets {
		out := Render(Preset(name), sampleContext())
		if strings.TrimSpace(out.Prompt) == "" {
			t.Errorf("preset %q rendered an empty prompt", name)
		}
	}
}

// The palette is the whole identity of these looks, so the signature
// colours are pinned as the truecolor SGR the engine should emit. A
// transcription that drifts is worse than no transcription: it ships
// under a name that promises someone else's design.
func TestGalleryPresetsEmitTheirUpstreamPalette(t *testing.T) {
	cases := []struct {
		preset string
		what   string
		sgr    string // ParseColor turns #rrggbb into "2;r;g;b"
	}{
		// starship pastel-powerline: directory is #DA627D.
		{"pastel-powerline", "directory pink", "2;218;98;125"},
		// starship tokyo-night: directory is #769ff0 on #e3e5e5 text.
		{"tokyo-night", "directory blue", "2;118;159;240"},
		{"tokyo-night", "directory text", "2;227;229;229"},
	}
	for _, tc := range cases {
		out := Render(Preset(tc.preset), sampleContext())
		if !strings.Contains(out.Prompt, tc.sgr) {
			t.Errorf("%s: %s (%s) not in the rendered prompt:\n%q",
				tc.preset, tc.what, tc.sgr, out.Prompt)
		}
	}
}

// A look ported from elsewhere either reproduces the upstream or says
// where it does not. Both starship presets lean heavily on the
// *_version module family, which nothing in this package will ever
// render, so both must declare it.
func TestStarshipPresetsDeclareWhatTheyDrop(t *testing.T) {
	for _, name := range []string{"pastel-powerline", "tokyo-night"} {
		cfg := Preset(name)
		if len(cfg.Unsupported) == 0 {
			t.Fatalf("preset %q claims full fidelity to starship; it cannot render the version modules", name)
		}
		for _, u := range cfg.Unsupported {
			if !strings.HasPrefix(u, "starship ") {
				t.Errorf("preset %q reports %q without naming the upstream it belongs to", name, u)
			}
		}
	}
}

// agnoster is rebuilt from its appearance rather than imported, because
// the oh-my-zsh theme is zsh code and there is nothing to import (#185).
// That distinction has to reach the user, not just this file's comments.
func TestAgnosterSaysItIsRebuiltNotImported(t *testing.T) {
	cfg := Preset("agnoster")
	joined := strings.Join(cfg.Unsupported, "\n")
	if !strings.Contains(joined, "rebuilt") {
		t.Errorf("agnoster does not disclose that it is a reconstruction: %q", joined)
	}
}
