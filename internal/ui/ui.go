// Package ui is the shared charm styling for koi's discrete surfaces
// (#90): doctor, the plugins/tool listings, the zi update board. The
// keystroke path (editor, completion menu, prompt) never goes through
// this package — decision #1 stands, the render loop stays koi-owned.
//
// Degradation is explicit, not sniffed: callers gate on Enabled(w),
// which is true only for a real terminal without NO_COLOR/dumb. A
// disabled palette is all identity styles, so the same render code
// emits plain text — piped output stays machine-readable, and tests
// (writing to builders) see exactly the unstyled strings.
package ui

import (
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"

	"github.com/blairham/koi-shell/internal/term"
)

// Enabled reports whether w is an interactive, color-willing terminal.
func Enabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f)
}

// Palette is the small shared vocabulary: status colors, emphasis, and
// the muted tone for fixes and metadata.
type Palette struct {
	OK, Warn, Fail lipgloss.Style
	Bold, Dim      lipgloss.Style
	Accent         lipgloss.Style
}

// Styles returns the palette; disabled means every style is the
// identity, so Render passes text through untouched.
func Styles(enabled bool) Palette {
	if !enabled {
		return Palette{}
	}
	return Palette{
		OK:     lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		Warn:   lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		Fail:   lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true),
		Bold:   lipgloss.NewStyle().Bold(true),
		Dim:    lipgloss.NewStyle().Faint(true),
		Accent: lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
	}
}
