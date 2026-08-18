package repl

import (
	"context"
	"sync"
	"time"

	"github.com/blairham/koi-shell/internal/pluginhost"
	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
)

// segmentRenderer resolves %p{id} prompt escapes against tier-2 prompt
// plugins, enforcing the docs/plugins.md contract: every render carries
// a deadline derived from the segment's declared budget, and a miss
// serves the segment's previous value (stale) or nothing — the prompt
// never waits.
//
// Discovery (launching prompt plugins and mapping segment ids) runs in
// the background from shell startup (#37: the first prompt must never
// pay plugin-launch latency). The first render waits for warm-up only
// within its own budget; segments missing at first paint appear on the
// next prompt.
type segmentRenderer struct {
	host *pluginhost.Host

	warmed chan struct{} // closed when discovery finishes

	mu       sync.Mutex
	segments map[string]segmentEntry
	last     map[string]string // stale fallback per segment id
}

type segmentEntry struct {
	client  pluginapi.PromptSegmentProviderClient
	budget  time.Duration
	envKeys []string
}

func newSegmentRenderer(host *pluginhost.Host) *segmentRenderer {
	r := &segmentRenderer{
		host:     host,
		warmed:   make(chan struct{}),
		segments: map[string]segmentEntry{},
		last:     map[string]string{},
	}
	go r.warm()
	return r
}

// warm maps segment ids to their providers once, launching prompt
// plugins off the startup path.
func (r *segmentRenderer) warm() {
	defer close(r.warmed)
	ctx, cancel := context.WithTimeout(context.Background(), pluginhost.DescribeTimeout)
	defer cancel()
	for _, prov := range r.host.PromptProviders(ctx) {
		resp, err := prov.Client.Segments(ctx, &pluginapi.SegmentsRequest{})
		if err != nil {
			continue
		}
		r.mu.Lock()
		for _, seg := range resp.GetSegments() {
			budget := pluginhost.DefaultRenderBudget
			if ms := seg.GetBudgetMs(); ms > 0 {
				budget = time.Duration(ms) * time.Millisecond
			}
			// First provider claiming an id wins (sorted by plugin name).
			if _, taken := r.segments[seg.GetId()]; !taken {
				r.segments[seg.GetId()] = segmentEntry{
					client: prov.Client, budget: budget, envKeys: seg.GetEnvKeys(),
				}
			}
		}
		r.mu.Unlock()
	}
}

// render returns the segment's current text. Unknown ids render empty;
// budget misses render the previous value. If discovery is still warming
// up, render waits for it only within the default budget.
func (r *segmentRenderer) render(
	ctx context.Context, id, cwd string, lastExit int, envFor func(keys []string) map[string]string,
) string {
	select {
	case <-r.warmed:
	case <-time.After(pluginhost.DefaultRenderBudget):
		return r.stale(id) // warm-up outran the budget: paint now, fill next prompt
	case <-ctx.Done():
		return r.stale(id)
	}

	r.mu.Lock()
	entry, ok := r.segments[id]
	r.mu.Unlock()
	if !ok {
		return ""
	}
	var env map[string]string
	if len(entry.envKeys) > 0 && envFor != nil {
		env = envFor(entry.envKeys)
	}
	rctx, cancel := context.WithTimeout(ctx, entry.budget)
	defer cancel()
	resp, err := entry.client.Render(rctx, &pluginapi.RenderRequest{
		SegmentId:    id,
		Cwd:          cwd,
		LastExitCode: int32(lastExit),
		EventSeq:     r.host.NextSeq(),
		Env:          env,
	})
	if err != nil {
		return r.stale(id)
	}
	r.mu.Lock()
	r.last[id] = resp.GetText()
	r.mu.Unlock()
	return resp.GetText()
}

func (r *segmentRenderer) stale(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[id]
}
