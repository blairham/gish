package main

import (
	"strings"
	"testing"
	"time"
)

// The p10k theme is the heaviest prompt gish ships: a dozen segments,
// a directory walk for each version manager, a git head read. Upstream
// answers that weight with an "instant prompt" — a cached prompt painted
// before anything is resolved, because in zsh the real one is not ready
// for tens of milliseconds.
//
// This test is what decides whether gish needs that machinery. If the
// themed first prompt lands inside the same budget as the naked one,
// there is nothing to hide behind a cache, and the honest thing is to
// not build the cache.
func TestStartupBudgetWithP10kTheme(t *testing.T) {
	if testing.Short() {
		t.Skip("startup benchmark skipped in -short")
	}
	bin := buildGish(t)
	env := append(hermeticEnv(t), "GISH_THEME=p10k")

	best := time.Hour
	var runs []string
	for range 5 {
		d := timeToFirstPrompt(t, bin, env)
		runs = append(runs, d.Round(time.Millisecond).String())
		if d < best {
			best = d
		}
	}
	t.Logf("p10k startup runs: %s (best %s, budget %s)",
		strings.Join(runs, " "), best.Round(time.Millisecond), startupBudget)
	if best > startupBudget {
		t.Fatalf("p10k startup regression: best of 5 = %s > budget %s", best, startupBudget)
	}
}
