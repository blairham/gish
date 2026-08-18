package repl

import (
	"context"
	"time"

	"github.com/blairham/koi-shell/internal/history"
	"github.com/blairham/koi-shell/internal/pluginhost"
	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
)

// fanoutHistory delivers a stored entry to every HistoryBackend plugin —
// asynchronously and deadline-bounded, fire-and-forget per
// docs/plugins.md: the next prompt never waits on a backend, and a
// backend's stored=false response governs only its own store. Entries
// arrive already scrubbed (the shell's store is the gate).
func fanoutHistory(host *pluginhost.Host, e history.Entry) {
	if host == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for _, prov := range host.HistoryBackends(ctx) {
			_, _ = prov.Client.Append(ctx, &pluginapi.AppendRequest{ //nolint:errcheck // fire-and-forget
				Entry: &pluginapi.HistoryEntry{
					Command:       e.Command,
					StartedUnixMs: e.StartedUnixMs,
					DurationMs:    e.DurationMs,
					ExitCode:      int32(e.ExitCode),
					Cwd:           e.Cwd,
					SessionId:     e.SessionID,
				},
			})
		}
	}()
}
