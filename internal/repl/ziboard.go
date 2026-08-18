package repl

import (
	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/koi-shell/internal/plugmgr"
	"github.com/blairham/koi-shell/internal/ui"
)

// ziUpdate routes `zi update` through the live board (#90) when the
// manager exposes progress and stdout is an interactive terminal;
// otherwise the classic in-order line output — piped zi update stays
// machine-readable.
func ziUpdate(mgr plugmgr.Manager, target string, hc interp.HandlerContext) error {
	prog, ok := mgr.(plugmgr.ProgressUpdater)
	if !ok || !ui.Enabled(hc.Stdout) {
		return mgr.Update(target, hc.Stdout)
	}
	return ui.RunBoard(hc.Stdout, hc.Stdin, func(emit func(ui.BoardEvent)) error {
		return prog.UpdateWithProgress(target, hc.Stdout, func(ev plugmgr.UpdateEvent) {
			emit(ui.BoardEvent{
				Index:   ev.Index,
				Name:    ev.Name,
				Started: ev.State == plugmgr.UpdateStarted,
				Done:    ev.State == plugmgr.UpdateDone,
				Outcome: ev.Outcome,
				Failed:  ev.Failed,
			})
		})
	})
}
