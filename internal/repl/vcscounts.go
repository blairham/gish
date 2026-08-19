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
//
// Serving cached numbers is only honest if the prompt admits when they
// have stopped being refreshed, and it did not (#132): the engine dims
// the segment when GitStatus.Stale is set, and nothing here ever set it,
// so counts from a scan that had been failing for a minute rendered
// exactly like counts from a scan that finished a moment ago. Hence the
// two clocks below.

// countsTTL is how long a scan's answer is served before a refresh is
// started. Short enough that the counters track editing, long enough
// that holding Enter does not fork per prompt.
const countsTTL = 900 * time.Millisecond

// countsStaleAfter is when the counters stop claiming to be current and
// render the dim marker instead.
//
// It is deliberately not countsTTL. The TTL is when a refresh *starts*,
// which in interactive use is almost every prompt — anyone who pauses a
// second between commands crosses it — so marking at the TTL would put
// the marker on nearly every prompt, and a marker that is always on says
// nothing. This is the other threshold: a refresh begins at countsTTL
// and is bounded at ScanTimeout, so by the sum of the two a scan has
// either delivered or given up. Numbers older than that survived a whole
// cycle that failed to replace them, which is the case the marker is
// for.
const countsStaleAfter = countsTTL + gitstatus.ScanTimeout

// countsCache holds the last scan per working tree.
var countsCache sync.Map // workTree -> *countsEntry

type countsEntry struct {
	mu       sync.Mutex
	counts   gitstatus.Counts
	scanning bool

	// fetched is the last *attempt* and succeeded is the last one that
	// produced counts. They are separate because a failing repository
	// moves only the first: the attempt time has to advance on failure
	// or a repository git cannot read forks once per prompt, but letting
	// that advance the freshness clock too is how counts from a scan
	// that has not worked in a minute keep rendering as current.
	fetched   time.Time
	succeeded time.Time
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
	if time.Since(e.fetched) > countsTTL && !e.scanning {
		e.scanning = true
		go e.scan(g.Dir)
	}
	have, counts := e.haveCount, e.counts
	stale := time.Since(e.succeeded) > countsStaleAfter
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
		Stale:      stale,
	})
}

// scan refreshes one working tree's counts off the prompt path.
func (e *countsEntry) scan(dir string) {
	counts, err := gitstatus.Read(context.Background(), dir)

	e.mu.Lock()
	defer e.mu.Unlock()
	e.scanning = false
	// The attempt time moves even on failure, so a repository git cannot
	// read does not turn into a fork per prompt.
	now := time.Now()
	e.fetched = now
	if err != nil {
		return
	}
	e.counts, e.haveCount, e.succeeded = counts, true, now
}
