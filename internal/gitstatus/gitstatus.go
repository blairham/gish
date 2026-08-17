// Package gitstatus is the working-tree scan behind the git prompt
// segments: the expensive half that HeadStatus deliberately leaves out.
//
// It exists because there were nearly two of it. cmd/gish-git already
// ran `git status --porcelain=v2 --branch` and parsed it for the
// %p{git} escape, while the native p10k theme rendered counters that
// nothing ever filled — internal/p10k's MergeCounts had no caller at
// all, so `vcs` could not show dirty, staged, untracked or stashed state
// in any repository. Writing a second parser would have been the third
// copy of a rule in this codebase in a week.
//
// The scan forks; that is why it is here and not on the prompt path. The
// rule the p10k engine is built on is that no segment forks while
// rendering, so callers run this in the background and render whatever
// they last had — stale counts beat a prompt that waits on git.
package gitstatus

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// scanTimeout bounds one scan. A repository large enough to exceed it
// is a repository where a prompt should not be waiting anyway.
const scanTimeout = 5 * time.Second

// Counts is a working tree's state.
type Counts struct {
	Branch     string
	Ahead      int
	Behind     int
	Staged     int
	Dirty      int
	Untracked  int
	Conflicted int
	Stashed    int
}

// Read scans the working tree at root.
func Read(ctx context.Context, root string) (Counts, error) {
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v2", "--branch")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return Counts{}, err
	}
	c := Parse(out)
	c.Stashed = StashCount(root)
	return c, nil
}

// Parse reads `git status --porcelain=v2 --branch` output.
//
//	# branch.head main            → branch (or "(detached)")
//	# branch.ab +1 -2             → ahead/behind
//	1/2 XY … / u …                → staged (X≠.) and worktree-dirty (Y≠.)
//	? path                        → untracked
func Parse(out []byte) Counts {
	var c Counts
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			c.Branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.ab "):
			for f := range strings.FieldsSeq(strings.TrimPrefix(line, "# branch.ab ")) {
				n, err := strconv.Atoi(f[1:])
				if err != nil {
					continue
				}
				if f[0] == '+' {
					c.Ahead = n
				} else {
					c.Behind = n
				}
			}
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			// XY at fields[1]: X is the index state, Y the worktree's.
			if fields := strings.Fields(line); len(fields) > 1 && len(fields[1]) == 2 {
				if fields[1][0] != '.' {
					c.Staged++
				}
				if fields[1][1] != '.' {
					c.Dirty++
				}
			}
		case strings.HasPrefix(line, "u "):
			// Unmerged is counted separately as well as being dirty: a
			// conflict is the state a prompt most needs to shout about,
			// and p10k gives it its own color.
			c.Conflicted++
			c.Dirty++
		case strings.HasPrefix(line, "? "):
			c.Untracked++
		}
	}
	return c
}

// StashCount reports how many stash entries the repository has.
//
// Read from the reflog rather than by running `git stash list`, because
// it is a line count of one small file and this is called alongside a
// scan that already forks once. porcelain=v2 has no stash field at all,
// which is why it needs its own answer rather than one more parse case.
func StashCount(root string) int {
	dir, err := gitDir(root)
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(dir, "logs", "refs", "stash"))
	if err != nil {
		return 0 // no stashes is the overwhelmingly common case
	}
	return bytes.Count(data, []byte("\n"))
}

// gitDir resolves root's .git, following the file form that worktrees
// and submodules use so those report their stashes too.
func gitDir(root string) (string, error) {
	candidate := filepath.Join(root, ".git")
	fi, err := os.Lstat(candidate)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return candidate, nil
	}
	data, err := os.ReadFile(candidate)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if target == "" {
		return "", os.ErrNotExist
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	return target, nil
}
