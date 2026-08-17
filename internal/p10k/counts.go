package p10k

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The vcs counters' producer (#130).
//
// The segment could already render ahead/behind, staged, modified,
// untracked, stashed and conflicted — colored correctly, wired through
// MergeCounts, and never populated, because nothing produced them. That
// split was deliberate (#129): the two halves of git status have wildly
// different costs. The branch is one file read cached on .git/HEAD's
// mtime and must never be late; the counts need a full index-versus-
// working-tree walk, which is the expensive part gitstatusd exists to
// make fast and the reason upstream gets them from another process.
//
// This is the third option the issue did not list, and measuring said
// it is the right one: run the scan **in the background, in process**,
// and let the prompt read whatever is current. `git status
// --porcelain=v2` is a subprocess, which the no-forking rule forbids —
// but that rule is about the *prompt path*, and nothing here is on it.
// The prompt takes a cached answer or none, never a wait.
//
// The alternative — reimplementing the index walk natively — is a real
// project (it is what gitstatusd is), and the alternative to *that* is
// a plugin hop that costs more than the scan does on a small repo.
// Neither buys anything the prompt can feel.

// countScanTimeout bounds one scan. A repository large enough to take
// longer is one where the counts will be stale anyway; the marker says
// so rather than the prompt waiting.
const countScanTimeout = 5 * time.Second

// countsEntry is one repository's last known counts.
type countsEntry struct {
	status  GitStatus
	mtime   time.Time // the .git mtime the scan was made against
	scanned bool
}

// CountScanner keeps per-repository counts fresh in the background.
//
// Zero value is usable. Safe for concurrent use: the prompt reads it on
// the main goroutine while scans finish on their own.
type CountScanner struct {
	mu       sync.Mutex
	cache    map[string]countsEntry
	inflight map[string]bool
}

// Counts returns what is known about the repository containing dir, and
// starts a refresh when the working tree has changed since the last
// scan. ok=false means nothing is known yet — the first prompt in a
// repository shows the branch, and the counts arrive on the next one.
func (s *CountScanner) Counts(dir string) (GitStatus, bool) {
	gitDir, workTree, ok := findGitDir(dir)
	if !ok {
		return GitStatus{}, false
	}
	mtime := dirMtime(gitDir)

	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]countsEntry{}
		s.inflight = map[string]bool{}
	}
	entry, have := s.cache[workTree]
	stale := !have || !entry.mtime.Equal(mtime)
	if stale && !s.inflight[workTree] {
		s.inflight[workTree] = true
		go s.scan(workTree, mtime)
	}
	s.mu.Unlock()

	if !have {
		return GitStatus{}, false
	}
	status := entry.status
	// Say so rather than presenting a cached answer as current: the
	// segment appends a dim marker, which is the honest rendering of
	// "these counts describe the tree as it was a moment ago".
	status.Stale = stale
	return status, true
}

// scan runs the expensive half and stores it.
func (s *CountScanner) scan(workTree string, mtime time.Time) {
	defer func() {
		s.mu.Lock()
		delete(s.inflight, workTree)
		s.mu.Unlock()
	}()

	status, err := scanCounts(workTree)
	if err != nil {
		return // no git, not a repo, or a scan that timed out: keep what we had
	}
	status.Dir = workTree

	s.mu.Lock()
	s.cache[workTree] = countsEntry{status: status, mtime: mtime, scanned: true}
	s.mu.Unlock()
}

// dirMtime is the invalidation key: git touches the index and the refs
// under .git for every operation that changes what the counts would
// say, so its mtime moving is the cheapest available "something
// happened here".
func dirMtime(gitDir string) time.Time {
	var newest time.Time
	for _, name := range []string{"", "index", "HEAD", "refs"} {
		fi, err := os.Stat(filepath.Join(gitDir, name))
		if err != nil {
			continue
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// scanCounts runs git's own status and parses it.
//
// porcelain=v2 rather than v1 because it is the stable machine format
// and carries the ahead/behind header — v1's `##` line has to be
// parsed out of a human string that changes with locale.
func scanCounts(workTree string) (GitStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), countScanTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v2", "--branch", "--show-stash")
	cmd.Dir = workTree
	// A prompt scan must not be steered by the user's aliases or by a
	// pager that never exits.
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat")
	out, err := cmd.Output()
	if err != nil {
		return GitStatus{}, err
	}
	return parseCounts(out), nil
}

// parseCounts reads porcelain v2:
//
//	# branch.ab +1 -2       ahead/behind the upstream
//	# stash 3               stashed entries
//	1/2 XY …                staged when X≠'.', modified when Y≠'.'
//	u …                     unmerged: a conflict
//	? …                     untracked
func parseCounts(out []byte) GitStatus {
	var s GitStatus
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# branch.ab "):
			for _, f := range strings.Fields(strings.TrimPrefix(line, "# branch.ab ")) {
				n, err := strconv.Atoi(f[1:])
				if err != nil {
					continue
				}
				switch f[0] {
				case '+':
					s.Ahead = n
				case '-':
					s.Behind = n
				}
			}
		case strings.HasPrefix(line, "# stash "):
			s.Stashed, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "# stash ")))
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			fields := strings.Fields(line)
			if len(fields) < 2 || len(fields[1]) != 2 {
				continue
			}
			if fields[1][0] != '.' {
				s.Staged++
			}
			if fields[1][1] != '.' {
				s.Modified++
			}
		case strings.HasPrefix(line, "u "):
			s.Conflicted++
		case strings.HasPrefix(line, "? "):
			s.Untracked++
		}
	}
	return s
}
