package repl

import (
	"context"
	"sync"
	"time"

	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

// segmentRenderer resolves %p{id} prompt escapes against tier-2 prompt
// plugins, enforcing the docs/plugins.md contract: every render carries
// a deadline derived from the segment's declared budget, and a miss
// serves the segment's previous value (stale) or nothing — the prompt
// never waits.
type segmentRenderer struct {
	host *pluginhost.Host

	mu         sync.Mutex
	discovered bool
	segments   map[string]segmentEntry
	last       map[string]string // stale fallback per segment id
}

type segmentEntry struct {
	client pluginapi.PromptSegmentProviderClient
	budget time.Duration
}

func newSegmentRenderer(host *pluginhost.Host) *segmentRenderer {
	return &segmentRenderer{
		host:     host,
		segments: map[string]segmentEntry{},
		last:     map[string]string{},
	}
}

// discover maps segment ids to their providers, once per session. This
// launches prompt plugins — the cost lands on the first prompt that
// actually uses a %p escape, never on shells that don't.
func (r *segmentRenderer) discover(ctx context.Context) {
	if r.discovered {
		return
	}
	r.discovered = true
	for _, prov := range r.host.PromptProviders(ctx) {
		sctx, cancel := context.WithTimeout(ctx, pluginhost.DescribeTimeout)
		resp, err := prov.Client.Segments(sctx, &pluginapi.SegmentsRequest{})
		cancel()
		if err != nil {
			continue
		}
		for _, seg := range resp.GetSegments() {
			budget := pluginhost.DefaultRenderBudget
			if ms := seg.GetBudgetMs(); ms > 0 {
				budget = time.Duration(ms) * time.Millisecond
			}
			// First provider claiming an id wins (sorted by plugin name).
			if _, taken := r.segments[seg.GetId()]; !taken {
				r.segments[seg.GetId()] = segmentEntry{client: prov.Client, budget: budget}
			}
		}
	}
}

// render returns the segment's current text. Unknown ids render empty;
// budget misses render the previous value.
func (r *segmentRenderer) render(ctx context.Context, id, cwd string, lastExit int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discover(ctx)
	entry, ok := r.segments[id]
	if !ok {
		return ""
	}
	rctx, cancel := context.WithTimeout(ctx, entry.budget)
	defer cancel()
	resp, err := entry.client.Render(rctx, &pluginapi.RenderRequest{
		SegmentId:    id,
		Cwd:          cwd,
		LastExitCode: int32(lastExit),
		EventSeq:     r.host.NextSeq(),
	})
	if err != nil {
		return r.last[id] // stale beats blocking
	}
	r.last[id] = resp.GetText()
	return resp.GetText()
}
