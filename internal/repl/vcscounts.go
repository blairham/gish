package repl

import (
	"context"
	"sync"
	"time"

	"github.com/blairham/koi-shell/internal/gitstatus"
	"github.com/blairham/koi-shell/internal/promptengine"
)

// The working-tree counts behind the p10k vcs segment (#52).
//
// internal/promptengine splits git status in two on purpose: HeadStatus reads
// the branch from one small file and must never be late, while the
// counts need a walk of the index against the working tree and arrive
// from "whoever owns the scan". Nobody did — MergeCounts had no caller —
// so `vcs` rendered a branch and never a dirty, staged, untracked or
// stashed marker, in any repository.
//
// This is that owner. The rule it has to keep is the one the whole
// engine is built on: no segment forks while rendering. So the prompt
// takes whatever the cache already holds and, when that is missing or
// old, starts a refresh in the background and renders without it. A
// prompt one keystroke behind on a counter is the cost; a prompt that
// waits on git is not acceptable at all.

// countsTTL is how long a scan's answer is served before a refresh is
// started. Short enough that the counters track editing, long enough
// that holding Enter does not fork per prompt.
const countsTTL = 900 * time.Millisecond

// countsCache holds the last scan per working tree.
var countsCache sync.Map // workTree -> *countsEntry

type countsEntry struct {
	mu        sync.Mutex
	counts    gitstatus.Counts
	fetched   time.Time
	scanning  bool
	haveCount bool
}

// mergeVCSCounts fills g's counters from the cache, starting a
// background scan when what is cached is missing or stale.
//
// g is left alone when nothing has been scanned yet, which is why the
// first prompt in a repository shows a branch and no counters rather
// than zeros — zeros would claim the tree is clean, and a wrong "clean"
// is worse than an absent counter.
func mergeVCSCounts(g *promptengine.GitStatus) {
	if g == nil || g.Dir == "" {
		return
	}
	v, _ := countsCache.LoadOrStore(g.Dir, &countsEntry{})
	e := v.(*countsEntry)

	e.mu.Lock()
	stale := time.Since(e.fetched) > countsTTL
	if stale && !e.scanning {
		e.scanning = true
		go e.scan(g.Dir)
	}
	have, counts := e.haveCount, e.counts
	e.mu.Unlock()

	if !have {
		return
	}
	g.MergeCounts(promptengine.GitStatus{
		Dir:        g.Dir,
		Ahead:      counts.Ahead,
		Behind:     counts.Behind,
		Staged:     counts.Staged,
		Modified:   counts.Dirty,
		Untracked:  counts.Untracked,
		Conflicted: counts.Conflicted,
		Stashed:    counts.Stashed,
	})
}

// scan refreshes one working tree's counts off the prompt path.
func (e *countsEntry) scan(dir string) {
	counts, err := gitstatus.Read(context.Background(), dir)

	e.mu.Lock()
	defer e.mu.Unlock()
	e.scanning = false
	// The timestamp moves even on failure, so a repository git cannot
	// read does not turn into a fork per prompt.
	e.fetched = time.Now()
	if err != nil {
		return
	}
	e.counts, e.haveCount = counts, true
}
