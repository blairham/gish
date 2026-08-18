package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/blairham/koi-shell/internal/gitstatus"
)

// gitStatus is one parsed snapshot of a repository's state.
//
// The scan and its parser live in internal/gitstatus, shared with the
// native p10k theme. There were nearly two copies of that parser: this
// one fed the %p{git} escape while p10k's vcs segment rendered counters
// nothing filled. One parser, two consumers — the plugin adds only how
// it wants the result rendered.
type gitStatus = gitstatus.Counts

// text renders the segment: branch, then only the non-zero counters.
func renderStatus(s gitStatus) string {
	var b strings.Builder
	b.WriteString(s.Branch)
	if s.Ahead > 0 {
		fmt.Fprintf(&b, " ↑%d", s.Ahead)
	}
	if s.Behind > 0 {
		fmt.Fprintf(&b, " ↓%d", s.Behind)
	}
	if s.Stashed > 0 {
		fmt.Fprintf(&b, " *%d", s.Stashed)
	}
	if s.Staged > 0 {
		fmt.Fprintf(&b, " +%d", s.Staged)
	}
	if s.Dirty > 0 {
		fmt.Fprintf(&b, " !%d", s.Dirty)
	}
	if s.Untracked > 0 {
		fmt.Fprintf(&b, " ?%d", s.Untracked)
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
	return gitstatus.Read(ctx, root)
}
