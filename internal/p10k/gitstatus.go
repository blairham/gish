package p10k

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Where git status comes from, and why it is split in two.
//
// A prompt needs two very different things from a repository. The branch
// name is one small file read and it must never be late — it is the part
// people navigate by. The counts (modified, untracked, ahead, behind)
// require walking the index against the working tree, which is the
// expensive part that gitstatusd exists to make fast, and which upstream
// gets from a *separate process* for exactly that reason.
//
// So: HeadStatus reads the cheap half natively and synchronously, and
// the counts arrive from whoever owns the scan (the gish-git plugin, a
// background refresh) and are merged in. A prompt with no scanner still
// shows the branch; it just shows no counts. It never waits for either.

// headCache remembers the last answer per repository, keyed on the
// mtime of the files that would change it.
var headCache sync.Map // gitDir -> headEntry

type headEntry struct {
	stamp  time.Time
	status GitStatus
}

// HeadStatus reports the repository state that can be had cheaply from
// the given directory: which branch or commit is checked out, and
// whether an operation is in progress. It returns nil when dir is not
// inside a repository.
//
// Cost is a handful of stats and one small read, cached on the mtime of
// .git/HEAD, so repeated prompts in one directory cost a single stat.
func HeadStatus(dir string) *GitStatus {
	gitDir, workTree, ok := findGitDir(dir)
	if !ok {
		return nil
	}

	headPath := filepath.Join(gitDir, "HEAD")
	fi, err := os.Stat(headPath)
	if err != nil {
		return nil
	}
	if cached, hit := headCache.Load(gitDir); hit {
		if entry := cached.(headEntry); entry.stamp.Equal(fi.ModTime()) {
			status := entry.status
			return &status
		}
	}

	data, err := os.ReadFile(headPath)
	if err != nil {
		return nil
	}
	status := GitStatus{Dir: workTree}
	head := strings.TrimSpace(string(data))
	if ref, isRef := strings.CutPrefix(head, "ref:"); isRef {
		status.Branch = strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	} else if len(head) >= 8 {
		// Detached: show enough of the hash to be useful, no more.
		status.Commit = head[:8]
	}
	status.Action = inProgress(gitDir)

	headCache.Store(gitDir, headEntry{stamp: fi.ModTime(), status: status})
	return &status
}

// inProgress names the operation the repository is in the middle of, if
// any. These are the states where a prompt earns its keep — it is the
// difference between "why is my branch weird" and knowing you are three
// commits into a rebase.
func inProgress(gitDir string) string {
	for _, probe := range []struct{ path, name string }{
		{"rebase-merge", "rebase"},
		{"rebase-apply", "rebase"},
		{"MERGE_HEAD", "merge"},
		{"CHERRY_PICK_HEAD", "cherry-pick"},
		{"REVERT_HEAD", "revert"},
		{"BISECT_LOG", "bisect"},
	} {
		if _, err := os.Lstat(filepath.Join(gitDir, probe.path)); err == nil {
			return probe.name
		}
	}
	return ""
}

// findGitDir walks up from dir looking for a repository, returning the
// git directory and the working tree root. It handles the .git *file*
// that worktrees and submodules use, so those get a prompt too.
func findGitDir(dir string) (gitDir, workTree string, ok bool) {
	for {
		candidate := filepath.Join(dir, ".git")
		fi, err := os.Lstat(candidate)
		switch {
		case err == nil && fi.IsDir():
			return candidate, dir, true
		case err == nil:
			// A file: "gitdir: <path>", absolute or relative to dir.
			data, readErr := os.ReadFile(candidate)
			if readErr != nil {
				return "", "", false
			}
			target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
			if target == "" {
				return "", "", false
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			return target, dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

// MergeCounts folds a scanner's expensive half into a head status. The
// counts are only applied when they describe the same working tree —
// a status for the repository you just left is worse than no counts.
func (g *GitStatus) MergeCounts(counts GitStatus) {
	if g == nil || counts.Dir != g.Dir {
		return
	}
	g.Ahead, g.Behind = counts.Ahead, counts.Behind
	g.PushAhead, g.PushBehind = counts.PushAhead, counts.PushBehind
	g.Stashed, g.Conflicted = counts.Stashed, counts.Conflicted
	g.Staged, g.Modified, g.Untracked = counts.Staged, counts.Modified, counts.Untracked
	g.Stale = counts.Stale
	if counts.RemoteRef != "" {
		g.RemoteRef = counts.RemoteRef
	}
	if counts.Tag != "" {
		g.Tag = counts.Tag
	}
}
