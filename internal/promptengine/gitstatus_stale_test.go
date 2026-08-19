package promptengine

import (
	"strings"
	"testing"
)

// TestVCSStaleMarkerRenders pins the marker the vcs segment appends when
// its counters are older than the producer could refresh them (#132).
//
// It had no test and no producer that set GitStatus.Stale, so the whole
// branch was unreachable: the field was written once, from another
// field that was never set. A marker nothing can trigger is worse than
// no marker, because the design reads as though freshness is handled.
func TestVCSStaleMarkerRenders(t *testing.T) {
	t.Parallel()

	render := func(stale bool) string {
		ctx := sampleContext()
		ctx.Git.Stale = stale
		cfg := Preset("lean")
		cfg.SetList("LEFT_PROMPT_ELEMENTS", []string{"vcs"})
		cfg.SetList("RIGHT_PROMPT_ELEMENTS", nil)
		return plain(Render(cfg, ctx).Prompt)
	}

	current := render(false)
	if strings.Contains(current, "…") {
		t.Errorf("counters that are current carry the stale marker: %q", current)
	}
	// The counters still render: stale means old, not withheld.
	if !strings.Contains(current, "!3") {
		t.Fatalf("vcs counters missing from %q", current)
	}

	old := render(true)
	if !strings.Contains(old, "…") {
		t.Errorf("stale counters presented as current: %q", old)
	}
	if !strings.Contains(old, "!3") {
		t.Errorf("stale counters withheld rather than marked: %q", old)
	}
}
