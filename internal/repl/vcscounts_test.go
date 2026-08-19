package repl

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/blairham/koi-shell/internal/gitstatus"
	"github.com/blairham/koi-shell/internal/promptengine"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		// No personal git config: a global merge.ff or template setting
		// must not decide whether this test passes.
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestMergeVCSCountsFillsTheSegment is the regression test for #52: the
// p10k vcs segment rendered counters that nothing supplied, because
// MergeCounts had no caller anywhere in the tree.
func TestMergeVCSCountsFillsTheSegment(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "f.txt")
	gitIn(t, dir, "commit", "-qm", "first")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := &promptengine.GitStatus{Dir: dir, Branch: "main"}

	// The first call starts the scan and returns immediately: the prompt
	// never waits on git. That is the behavior, not a limitation, so it
	// is asserted rather than slept through.
	mergeVCSCounts(g)
	if g.Modified != 0 {
		t.Errorf("first call blocked on the scan: %+v", g)
	}

	// The counts land once the background scan finishes.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		g = &promptengine.GitStatus{Dir: dir, Branch: "main"}
		mergeVCSCounts(g)
		if g.Modified > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("counts never arrived for a dirty tree: %+v", g)
}

// A directory that is not a repository must not be scanned, and must not
// be reported as clean-with-counts either.
func TestMergeVCSCountsIgnoresNonRepos(t *testing.T) {
	t.Parallel()

	g := &promptengine.GitStatus{}
	mergeVCSCounts(g) // no Dir: nothing to do, and no panic
	if g.Modified != 0 || g.Stashed != 0 {
		t.Errorf("counts invented for an empty status: %+v", g)
	}
}

// TestMergeVCSCountsMarksCountsNothingRefreshed covers the other half of
// the cache's contract (#132). The engine renders a dim marker when
// GitStatus.Stale is set — and nothing in the tree ever set it, so a
// prompt served counts from a scan that had stopped working presented
// them as current. The two clocks are what make this decidable: the
// attempt time keeps a broken repository from forking per prompt, while
// the success time is the age of the numbers actually on screen.
func TestMergeVCSCountsMarksCountsNothingRefreshed(t *testing.T) {
	t.Parallel()

	// fetched is now in both cases, so no background scan starts and the
	// test neither forks nor sleeps: what is under test is the decision,
	// not the scanner.
	prime := func(dir string, lastGood time.Time) {
		countsCache.Store(dir, &countsEntry{
			counts:    gitstatus.Counts{Dirty: 3},
			fetched:   time.Now(),
			succeeded: lastGood,
			haveCount: true,
		})
	}

	fresh := t.TempDir()
	prime(fresh, time.Now())
	g := &promptengine.GitStatus{Dir: fresh, Branch: "main"}
	mergeVCSCounts(g)
	if g.Modified != 3 {
		t.Fatalf("cached counts not served: %+v", g)
	}
	if g.Stale {
		t.Errorf("counts from a scan that just succeeded marked stale: %+v", g)
	}

	// A repository git has stopped answering for: the attempt clock keeps
	// moving, so only the success clock can tell.
	stuck := t.TempDir()
	prime(stuck, time.Now().Add(-2*countsStaleAfter))
	g = &promptengine.GitStatus{Dir: stuck, Branch: "main"}
	mergeVCSCounts(g)
	if g.Modified != 3 {
		t.Fatalf("cached counts not served: %+v", g)
	}
	if !g.Stale {
		t.Errorf("counts no scan has replaced presented as current: %+v", g)
	}
}
