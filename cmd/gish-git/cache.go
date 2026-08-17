package main

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// worktreeTTL bounds how stale worktree dirty-state may get: edits to
// tracked files don't touch .git until staged, so the watcher can't see
// them — the TTL covers that gap. Everything watcher-visible (branch
// switches, commits, staging) invalidates immediately.
const worktreeTTL = 2 * time.Second

// repoCache serves cached git status per repository, gitstatusd-style:
// renders are answered from cache immediately; refreshes run in the
// background (single-flight per repo), triggered by fsnotify events on
// the .git directory or by TTL expiry. Never polls.
type repoCache struct {
	mu    sync.Mutex
	repos map[string]*repoState
	roots map[string]string // dir → repo root ("" = known non-repo)
}

type repoState struct {
	root string

	mu         sync.Mutex
	status     gitStatus
	haveStatus bool
	updatedAt  time.Time
	stale      bool
	refreshing bool
	watcher    *fsnotify.Watcher
}

func newRepoCache() *repoCache {
	return &repoCache{
		repos: map[string]*repoState{},
		roots: map[string]string{},
	}
}

// lookup resolves the repo for dir, caching both hits and misses.
func (c *repoCache) lookup(ctx context.Context, dir string) *repoState {
	c.mu.Lock()
	root, known := c.roots[dir]
	c.mu.Unlock()
	if !known {
		root = repoRoot(ctx, dir)
		c.mu.Lock()
		c.roots[dir] = root
		c.mu.Unlock()
	}
	if root == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rs, ok := c.repos[root]
	if !ok {
		rs = &repoState{root: root, stale: true}
		rs.watch()
		c.repos[root] = rs
	}
	return rs
}

// watch subscribes to the .git directory: HEAD swaps, index writes, ref
// updates all land here. Best-effort — without a watcher the TTL alone
// drives refreshes.
func (rs *repoState) watch() {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return
	}
	gitDir := filepath.Join(rs.root, ".git")
	if err := w.Add(gitDir); err != nil {
		w.Close()
		return
	}
	// refs/heads changes (commits, branch moves) happen inside subdirs;
	// watch the refs dir too, ignoring errors (packed refs still show up
	// via .git itself).
	_ = w.Add(filepath.Join(gitDir, "refs", "heads")) //nolint:errcheck // optional watch
	rs.watcher = w
	go func() {
		for {
			select {
			case _, ok := <-w.Events:
				if !ok {
					return
				}
				rs.mu.Lock()
				rs.stale = true
				rs.mu.Unlock()
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	}()
}

// render returns the current segment text for dir. Fresh cache answers
// immediately; a stale cache answers with the previous value while a
// background refresh runs; only the very first render of a repo blocks,
// and only until ctx's deadline.
func (c *repoCache) render(ctx context.Context, dir string) string {
	rs := c.lookup(ctx, dir)
	if rs == nil {
		return ""
	}

	rs.mu.Lock()
	needsRefresh := rs.stale || time.Since(rs.updatedAt) > worktreeTTL
	first := !rs.haveStatus
	if needsRefresh && !rs.refreshing {
		rs.refreshing = true
		go rs.refresh()
	}
	text := renderStatus(rs.status)
	rs.mu.Unlock()

	if !first {
		return text // possibly one refresh behind; repaint catches up
	}

	// First render for this repo: wait for the refresh, bounded by the
	// caller's deadline (the host's render budget).
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(2 * time.Millisecond):
		}
		rs.mu.Lock()
		if rs.haveStatus {
			text := renderStatus(rs.status)
			rs.mu.Unlock()
			return text
		}
		rs.mu.Unlock()
	}
}

func (rs *repoState) refresh() {
	status, err := readStatus(context.Background(), rs.root)
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.refreshing = false
	if err != nil {
		rs.stale = false // don't hot-loop on a broken repo; TTL retries
		rs.updatedAt = time.Now()
		return
	}
	rs.status = status
	rs.haveStatus = true
	rs.stale = false
	rs.updatedAt = time.Now()
}
