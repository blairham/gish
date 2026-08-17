package p10k

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The counters' producer (#130). The invariant under test is the one
// the whole design turns on: the prompt renders what is known *now* and
// never waits for a scan.

func TestParseCountsReadsPorcelainV2(t *testing.T) {
	t.Parallel()

	out := []byte(`# branch.oid deadbeef
# branch.head main
# branch.upstream origin/main
# branch.ab +2 -3
# stash 4
1 M. N... 100644 100644 100644 aaa bbb staged.txt
1 .M N... 100644 100644 100644 aaa bbb modified.txt
2 R. N... 100644 100644 100644 aaa bbb R100 new.txt	old.txt
u UU N... 100644 100644 100644 100644 aaa bbb ccc conflict.txt
? untracked.txt
? another.txt
`)
	got := parseCounts(out)
	want := GitStatus{Ahead: 2, Behind: 3, Stashed: 4, Staged: 2, Modified: 1, Conflicted: 1, Untracked: 2}
	if got != want {
		t.Errorf("parseCounts = %+v, want %+v", got, want)
	}
}

// A first look at a repository answers "nothing known yet" rather than
// blocking, and the counts arrive for the prompt after.
func TestCountsAreBackgroundNotBlocking(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	repo := newTestRepo(t)
	var scanner CountScanner

	start := time.Now()
	if _, ok := scanner.Counts(repo); ok {
		t.Error("the first look reported counts it could not have had yet")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("the first look took %v — it waited for the scan", elapsed)
	}

	// The scan lands on its own; a later prompt sees it.
	deadline := time.Now().Add(10 * time.Second)
	var status GitStatus
	for {
		var ok bool
		status, ok = scanner.Counts(repo)
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background scan never produced counts")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status.Untracked != 1 {
		t.Errorf("untracked = %d, want 1 (%+v)", status.Untracked, status)
	}
	if status.Dir != repo {
		t.Errorf("counts carry Dir=%q, want %q — MergeCounts drops anything else", status.Dir, repo)
	}
}

// Counts for the repository you just left are worse than no counts, so
// MergeCounts refuses them. That guard is what makes a shared,
// per-repository cache safe to read from any directory.
func TestMergeCountsRefusesAnotherRepository(t *testing.T) {
	t.Parallel()

	head := &GitStatus{Dir: "/repo/a", Branch: "main"}
	head.MergeCounts(GitStatus{Dir: "/repo/b", Untracked: 9})
	if head.Untracked != 0 {
		t.Errorf("counts from another repository were applied: %+v", head)
	}
	head.MergeCounts(GitStatus{Dir: "/repo/a", Untracked: 9})
	if head.Untracked != 9 {
		t.Errorf("counts for this repository were not applied: %+v", head)
	}
}

func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// A developer's own git config must not steer the fixture.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v (%s)", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
