package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blairham/gish/internal/gitstatus"
)

func TestParseStatus(t *testing.T) {
	t.Parallel()

	out := []byte(`# branch.oid 1234
# branch.head main
# branch.upstream origin/main
# branch.ab +2 -1
1 M. N... 100644 100644 100644 aaaa bbbb staged.go
1 .M N... 100644 100644 100644 aaaa bbbb dirty.go
1 MM N... 100644 100644 100644 aaaa bbbb both.go
u UU N... 100644 100644 100644 100644 aaaa bbbb cccc conflict.go
? new.txt
? other.txt
`)
	s := gitstatus.Parse(out)
	// Conflicted counts as dirty too, so the unmerged entry lands in
	// both columns.
	want := gitStatus{Branch: "main", Ahead: 2, Behind: 1, Staged: 2, Dirty: 3, Untracked: 2, Conflicted: 1}
	if s != want {
		t.Errorf("parseStatus = %+v, want %+v", s, want)
	}
}

func TestStatusText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		s    gitStatus
		want string
	}{
		{gitStatus{Branch: "main"}, "main"},
		{gitStatus{Branch: "main", Ahead: 1, Dirty: 2}, "main ↑1 !2"},
		{gitStatus{Branch: "dev", Behind: 3, Staged: 1, Untracked: 4}, "dev ↓3 +1 ?4"},
		// Stashes were invisible before the scan was shared (#52):
		// porcelain v2 has no stash field at all.
		{gitStatus{Branch: "main", Stashed: 2}, "main *2"},
	}
	for _, tt := range tests {
		if got := renderStatus(tt.s); got != tt.want {
			t.Errorf("renderStatus(%+v) = %q, want %q", tt.s, got, tt.want)
		}
	}
}

// initRepo creates a real repository with one commit on branch "trunk".
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "init")
	return dir
}

func TestRepoCacheRendersBranch(t *testing.T) {
	t.Parallel()

	dir := initRepo(t)
	cache := newRepoCache()
	// A background scan must not outlive the temp directory: git
	// writes into .git, and cleanup then fails with "directory not
	// empty" — on the slower runner only.
	defer cache.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := cache.render(ctx, dir)
	if got != "trunk" {
		t.Errorf("render = %q, want %q", got, "trunk")
	}
}

func TestRepoCacheSeesNewUntrackedFile(t *testing.T) {
	t.Parallel()

	dir := initRepo(t)
	cache := newRepoCache()
	// A background scan must not outlive the temp directory: git
	// writes into .git, and cleanup then fails with "directory not
	// empty" — on the slower runner only.
	defer cache.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if got := cache.render(ctx, dir); got != "trunk" {
		t.Fatalf("initial render = %q", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Worktree-only changes are TTL-driven (the watcher can't see them);
	// renders serve stale then catch up after a background refresh.
	deadline := time.Now().Add(8 * time.Second)
	for {
		if got := cache.render(ctx, dir); strings.Contains(got, "?1") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("untracked file never appeared: %q", cache.render(ctx, dir))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRepoCacheNonRepo(t *testing.T) {
	t.Parallel()

	cache := newRepoCache()
	// A background scan must not outlive the temp directory: git
	// writes into .git, and cleanup then fails with "directory not
	// empty" — on the slower runner only.
	defer cache.close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if got := cache.render(ctx, t.TempDir()); got != "" {
		t.Errorf("non-repo render = %q, want empty", got)
	}
}
