package promptengine

import (
	"strings"
	"testing"
)

// TestDemoPresets is not an assertion, it is a viewer: run it with -v to
// see every preset rendered in the terminal. The wizard's preview uses
// the same path (Render against a canned context), so what this prints
// is what a user choosing a preset will be shown.
func TestDemoPresets(t *testing.T) {
	if testing.Short() {
		t.Skip("viewer only")
	}
	ctx := sampleContext()
	ctx.ExitCode = 1
	for _, name := range Presets() {
		got := Render(Preset(name), ctx)
		line := got.Prompt
		if got.RPrompt != "" {
			line += "   [rprompt] " + got.RPrompt
		}
		t.Logf("\n=== %s ===%s%s", name, line, "\x1b[0m")
		_ = strings.TrimSpace(line)
	}
}
