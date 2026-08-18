package repl

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/koi-shell/internal/plugmgr"
	"github.com/blairham/koi-shell/internal/ui"
)

// ziList renders installed objects: a lipgloss table on an interactive
// terminal, the classic lines when piped.
func ziList(mgr plugmgr.Manager, hc interp.HandlerContext) error {
	lister, ok := mgr.(plugmgr.ObjectLister)
	if !ok || !ui.Enabled(hc.Stdout) {
		return mgr.List(hc.Stdout)
	}
	objects, err := lister.Objects()
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		fmt.Fprintln(hc.Stdout, "nothing installed — zi load user/repo to start")
		return nil
	}
	style := ui.Styles(true)
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(style.Dim).
		Headers("KIND", "OBJECT", "ICES")
	for _, o := range objects {
		t.Row(style.Accent.Render(o.Kind), style.Bold.Render(o.Raw), style.Dim.Render(o.Ices))
	}
	fmt.Fprintln(hc.Stdout, t)
	return nil
}
