package repl

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/blairham/koi-shell/internal/history"
	"github.com/blairham/koi-shell/internal/pluginhost"
	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
)

// The read half of HistoryBackend (#97). Append already fans out
// (fanout.go); this is what makes a backend worth installing — ctrl-r
// that reaches commands this machine never ran.
//
// The shape follows the invariant the whole history design rests on:
// **the local store is authoritative and always answers first**. Backend
// results are additive, budget-bounded, and merged behind the local
// ones. A backend that is slow, broken, or absent costs the user reach,
// never the picker.

// searchBackends collects entries from every HistoryBackend within the
// budget. It never returns an error: every failure mode here — no
// plugins, a crashed plugin, a stream that misses the deadline — means
// the same thing to the caller, which is "use what you have".
func searchBackends(ctx context.Context, host *pluginhost.Host, query, cwd string, limit int, prefixOnly bool) []history.Entry {
	if host == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pluginhost.DefaultHistorySearchBudget)
	defer cancel()

	backends := host.HistoryBackends(ctx)
	if len(backends) == 0 {
		return nil
	}

	// Backends are queried concurrently: two slow backends should cost
	// one budget, not two. Results are collected under a mutex because
	// the budget may expire mid-flight and we take whatever arrived.
	var (
		mu  sync.Mutex
		out []history.Entry
		wg  sync.WaitGroup
	)
	req := &pluginapi.SearchRequest{
		Query:      query,
		Cwd:        cwd,
		Limit:      uint32(limit), //nolint:gosec // limit is a small positive constant from the caller
		PrefixOnly: prefixOnly,
	}
	for _, prov := range backends {
		wg.Add(1)
		go func(client pluginapi.HistoryBackendClient) {
			defer wg.Done()
			entries := drainSearch(ctx, client, req)
			if len(entries) == 0 {
				return
			}
			mu.Lock()
			out = append(out, entries...)
			mu.Unlock()
		}(prov.Client)
	}
	wg.Wait()
	return out
}

// drainSearch reads batches until the backend says final or the budget
// ends. Partial results are kept: docs/plugins.md's rule for ctrl-r is
// that whatever batches arrived render, best-first.
func drainSearch(ctx context.Context, client pluginapi.HistoryBackendClient, req *pluginapi.SearchRequest) []history.Entry {
	stream, err := client.Search(ctx, req)
	if err != nil {
		return nil
	}
	var out []history.Entry
	for {
		batch, err := stream.Recv()
		if err != nil { // io.EOF, a deadline, or a crashed plugin
			return out
		}
		for _, e := range batch.GetEntries() {
			if e.GetCommand() == "" {
				continue
			}
			out = append(out, history.Entry{
				Command:       e.GetCommand(),
				StartedUnixMs: e.GetStartedUnixMs(),
				DurationMs:    e.GetDurationMs(),
				ExitCode:      int(e.GetExitCode()),
				Cwd:           e.GetCwd(),
				SessionID:     e.GetSessionId(),
			})
		}
		if batch.GetFinal() {
			return out
		}
	}
}

// mergeHistory combines local and backend entries into one list.
//
// Local wins every tie, and the reason is not politeness: the local
// entry is the one whose metadata this machine actually observed. A
// backend's copy of the same command may carry another machine's cwd and
// another machine's clock, and showing that in the picker's detail
// column would be a quiet lie about where the command ran.
//
// Order is local-first, then backend entries newest-first. The picker
// applies its own fuzzy ranking on top; this only decides what ties
// resolve to and what gets dropped at the cap.
func mergeHistory(local, backend []history.Entry, limit int) []history.Entry {
	seen := make(map[string]struct{}, len(local)+len(backend))
	out := make([]history.Entry, 0, min(len(local)+len(backend), limit))

	for _, e := range local {
		if _, dup := seen[e.Command]; dup {
			continue
		}
		seen[e.Command] = struct{}{}
		out = append(out, e)
	}
	if len(backend) == 0 {
		return out
	}

	extra := slices.Clone(backend)
	// cmp.Compare, not a subtraction: the difference of two epoch
	// millisecond stamps does not fit an int on a 32-bit build.
	slices.SortStableFunc(extra, func(a, b history.Entry) int {
		return cmp.Compare(b.StartedUnixMs, a.StartedUnixMs)
	})
	for _, e := range extra {
		if len(out) >= limit {
			break
		}
		if _, dup := seen[e.Command]; dup {
			continue
		}
		seen[e.Command] = struct{}{}
		out = append(out, e)
	}
	return out
}

// remoteMark labels a row the local store did not have. Without it,
// commands this machine never ran showing up in ctrl-r reads as a bug
// rather than as the feature the user installed a backend to get.
const remoteMark = "synced"

// commandSet indexes entries by command, so the picker can tell which
// rows came from a backend.
func commandSet(entries []history.Entry) map[string]struct{} {
	set := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		set[e.Command] = struct{}{}
	}
	return set
}
