package promptengine

import (
	"strings"
	"testing"
)

// POWERLEVEL9K_MODE has to select something (#131): it was parsed,
// stored, imported and written back out while changing nothing.
func TestModeSelectsIcons(t *testing.T) {
	t.Parallel()

	ascii := Preset("lean")
	ascii.Set("MODE", "ascii")
	if got := ascii.Icon("kubecontext", "", "⎈"); got != "k8s" {
		t.Errorf("ascii kubecontext icon = %q, want k8s", got)
	}

	nerd := Preset("lean")
	nerd.Set("MODE", "nerdfont-v3")
	if got := nerd.Icon("kubecontext", "", ""); got != "⎈" {
		t.Errorf("nerdfont kubecontext icon = %q", got)
	}
}

// An explicit per-segment override still wins, which is what keeps an
// imported config's icons exact.
func TestExplicitIconBeatsTheTable(t *testing.T) {
	t.Parallel()

	cfg := Preset("lean")
	cfg.Set("MODE", "ascii")
	cfg.Set("KUBECONTEXT_VISUAL_IDENTIFIER_EXPANSION", "☸")
	if got := cfg.Icon("kubecontext", "", ""); got != "☸" {
		t.Errorf("override = %q, want the explicit glyph", got)
	}
}

// `status` is one segment with two icons; a table keyed only by segment
// name replaced the error mark with the ok one, which reads as a prompt
// that has stopped flagging failures.
func TestIconsAreStateAware(t *testing.T) {
	t.Parallel()

	cfg := Preset("lean")
	if got := cfg.Icon("status", "ERROR", "✘"); got != "✘" {
		t.Errorf("error icon = %q", got)
	}
	if got := cfg.Icon("status", "OK", "✔"); got != "✔" {
		t.Errorf("ok icon = %q", got)
	}
}

// A mode gish does not carry is served by the default one and says so,
// rather than silently substituting a different glyph set.
func TestUnknownModeFallsBackAudibly(t *testing.T) {
	t.Parallel()

	cfg := Preset("lean")
	cfg.Set("MODE", "nerdfont-complete")
	mode := cfg.ResolveIconMode()
	if mode.Serving != "nerdfont-v3" || !mode.Fallback() {
		t.Errorf("mode = %+v, want nerdfont-v3 serving as a fallback", mode)
	}

	cfg.Set("MODE", "something-invented")
	if mode := cfg.ResolveIconMode(); !mode.Fallback() {
		t.Errorf("an unknown mode did not report a fallback: %+v", mode)
	}

	cfg.Set("MODE", "ascii")
	if mode := cfg.ResolveIconMode(); mode.Fallback() {
		t.Errorf("a carried mode reported a fallback: %+v", mode)
	}
}

// MODE=ascii has to produce a prompt with no glyphs in it at all —
// that is the entire point of the mode, and it is what the wizard now
// relies on instead of writing per-segment overrides.
func TestAsciiModeRendersWithoutGlyphs(t *testing.T) {
	t.Parallel()

	ctx := sampleContext()
	ctx.ExitCode = 1
	cfg := Preset("lean")
	cfg.Set("MODE", "ascii")
	out := plain(Render(cfg, ctx).Prompt)
	for _, r := range out {
		// Private Use Area: where every patched-font glyph lives.
		if r >= 0xE000 && r <= 0xF8FF {
			t.Errorf("ascii mode rendered a private-use glyph %U in %q", r, out)
		}
	}
	if strings.ContainsAny(out, "✘✔") {
		t.Errorf("ascii mode kept the unicode status marks: %q", out)
	}
}
