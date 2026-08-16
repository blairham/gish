package repl

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/blairham/gish/internal/ui"
)

// The zi help screen (#90), styled after the original Zi's usage
// output: a ⟨ Zi ⟩ badge, an aligned command column, colorized
// argument hints. Plain text when piped — the same words either way.

type ziHelpRow struct {
	command string
	args    string // bracketed hints, upstream-Zi style
	desc    string
}

var ziHelp = []ziHelpRow{
	{"ice", "[ices…]", "buffer ice modifiers for the next load/snippet"},
	{"load|light", "[plugin]", "install if needed and load a plugin (user/repo or URL)"},
	{"snippet", "[snippet]|[url]", "install if needed and load a snippet (URL or OMZ:: alias)"},
	{"update", "[plugin]|[url]", "update one object, or everything concurrently"},
	{"delete", "[plugin]|[url]", "remove an installed object from the disk"},
	{"list|status", "", "show installed objects with their ices"},
}

const ziHelpFoot = `ices use Zi spelling: zi ice wait"1" lucid from"gh-r" pick"bin/fzf"
(wait is accepted but loads run immediately until idle hooks land)`

// pad right-pads styled text to width, measured on the plain form.
func pad(styled, plain string, width int) string {
	if n := width - lipgloss.Width(plain); n > 0 {
		return styled + strings.Repeat(" ", n)
	}
	return styled
}

// printZiHelp renders the usage screen; the styled and piped forms
// carry identical words.
func printZiHelp(w io.Writer) {
	style := ui.Styles(ui.Enabled(w))
	dash := style.Dim.Render("—")
	fmt.Fprintf(w, "%s %s %s %s\n",
		dash, style.Accent.Render("⟨ Zi ⟩"), dash, style.Bold.Render("Usage:"))
	for _, row := range ziHelp {
		// Pad by display width before styling: neither ANSI escapes nor
		// multi-byte runes may count against the column.
		fmt.Fprintf(w, "%s %s %s %s %s\n",
			style.Dim.Render("›"),
			pad(style.Accent.Render(row.command), row.command, 12),
			pad(style.OK.Render(row.args), row.args, 16),
			dash, row.desc)
	}
	fmt.Fprintln(w)
	for line := range strings.Lines(ziHelpFoot) {
		fmt.Fprintln(w, style.Dim.Render(strings.TrimSuffix(line, "\n")))
	}
}
