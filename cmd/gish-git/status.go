package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// gitStatus is one parsed snapshot of a repository's state.
type gitStatus struct {
	branch    string
	ahead     int
	behind    int
	staged    int
	dirty     int
	untracked int
}

// text renders the segment: branch, then only the non-zero counters.
func (s gitStatus) text() string {
	var b strings.Builder
	b.WriteString(s.branch)
	if s.ahead > 0 {
		fmt.Fprintf(&b, " ↑%d", s.ahead)
	}
	if s.behind > 0 {
		fmt.Fprintf(&b, " ↓%d", s.behind)
	}
	if s.staged > 0 {
		fmt.Fprintf(&b, " +%d", s.staged)
	}
	if s.dirty > 0 {
		fmt.Fprintf(&b, " !%d", s.dirty)
	}
	if s.untracked > 0 {
		fmt.Fprintf(&b, " ?%d", s.untracked)
	}
	return b.String()
}

// repoRoot resolves the repository top level for dir; empty when dir is
// not inside a work tree.
func repoRoot(ctx context.Context, dir string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readStatus runs `git status --porcelain=v2 --branch` and parses it.
func readStatus(ctx context.Context, root string) (gitStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v2", "--branch")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return gitStatus{}, err
	}
	return parseStatus(out), nil
}

// parseStatus parses porcelain v2 output.
//
//	# branch.head main            → branch (or "(detached)")
//	# branch.ab +1 -2             → ahead/behind
//	1/2 XY … / u …                → staged (X≠.) and worktree-dirty (Y≠.)
//	? path                        → untracked
func parseStatus(out []byte) gitStatus {
	var s gitStatus
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			s.branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.ab "):
			fields := strings.Fields(strings.TrimPrefix(line, "# branch.ab "))
			for _, f := range fields {
				n, err := strconv.Atoi(f[1:])
				if err != nil {
					continue
				}
				if f[0] == '+' {
					s.ahead = n
				} else {
					s.behind = n
				}
			}
		case strings.HasPrefix(line, "1 ") || strings.HasPrefix(line, "2 "):
			// XY at fields[1]: X = staged state, Y = worktree state.
			if fields := strings.Fields(line); len(fields) > 1 && len(fields[1]) == 2 {
				if fields[1][0] != '.' {
					s.staged++
				}
				if fields[1][1] != '.' {
					s.dirty++
				}
			}
		case strings.HasPrefix(line, "u "):
			s.dirty++ // unmerged counts as dirty
		case strings.HasPrefix(line, "? "):
			s.untracked++
		}
	}
	return s
}
