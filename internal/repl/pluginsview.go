package repl

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/internal/ui"
)

// printPluginStatuses renders the plugins listing: a lipgloss table on
// an interactive terminal, the classic machine-readable columns when
// piped (#90's degradation rule).
func printPluginStatuses(w io.Writer, statuses []pluginhost.Status, cmdIndex *pluginhost.CommandIndex) {
	rows := make([][2]string, 0, len(statuses)) // name → state, for styling
	type rowData struct{ name, state, version, caps, cmds string }
	data := make([]rowData, 0, len(statuses))
	for _, st := range statuses {
		state := "stopped"
		switch {
		case st.Running:
			state = "running"
		case time.Now().Before(st.BackoffUntil):
			state = "backoff"
		}
		cmds := ""
		if names := cmdIndex.CommandsOf(st.Name); len(names) > 0 {
			cmds = strings.Join(names, ", ")
		}
		data = append(data, rowData{st.Name, state, st.Version, strings.Join(st.Capabilities, ", "), cmds})
		rows = append(rows, [2]string{st.Name, state})
	}
	_ = rows

	if !ui.Enabled(w) {
		for _, d := range data {
			line := fmt.Sprintf("%-20s %-8s %-12s %s", d.name, d.state, d.version, d.caps)
			if d.cmds != "" {
				line += "  cmds: " + strings.ReplaceAll(d.cmds, ", ", ",")
			}
			fmt.Fprintln(w, line)
		}
		return
	}

	style := ui.Styles(true)
	stateStyle := map[string]lipgloss.Style{
		"running": style.OK, "backoff": style.Warn, "stopped": style.Dim,
	}
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(style.Dim).
		Headers("PLUGIN", "STATE", "VERSION", "CAPABILITIES", "COMMANDS")
	for _, d := range data {
		t.Row(style.Bold.Render(d.name), stateStyle[d.state].Render(d.state), d.version, d.caps, style.Dim.Render(d.cmds))
	}
	fmt.Fprintln(w, t)
}
