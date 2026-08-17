package gitstatus

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Against a real repository, because the value of this package is that
// it reads what git actually prints. A hand-written fixture would pin my
// reading of porcelain v2 rather than porcelain v2.

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv()...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitAllowFail runs git where a nonzero status is the expected outcome.
// gitEnv gives git an identity and, just as importantly, no user
// configuration.
//
// Without the config isolation these tests inherit whoever is running
// them: a global merge.ff=only setting made `git merge` refuse outright
// with status 128 rather than conflict, so the conflict test saw a clean
// tree and read as "conflicts are not counted". A test that depends on
// personal git config is a test that fails on somebody else's machine.
func gitEnv() []string {
	return []string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	}
}

func gitAllowFail(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv()...)
	_ = cmd.Run()
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadCountsEveryState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "tracked.txt", "one\n")
	git(t, dir, "add", "tracked.txt")
	git(t, dir, "commit", "-qm", "first")

	// The stash goes first, on purpose: `git stash push` takes every
	// change in the tree, so making the dirty and staged files before
	// stashing would stash the very things this asserts on. Writing the
	// order down because the first version did exactly that and reported
	// a clean tree.
	write(t, dir, "stashme.txt", "x\n")
	git(t, dir, "add", "stashme.txt")
	git(t, dir, "stash", "push", "-q", "-m", "wip")

	// Then one of each state the segment can show.
	write(t, dir, "tracked.txt", "modified\n") // dirty
	write(t, dir, "staged.txt", "new\n")
	git(t, dir, "add", "staged.txt") // staged
	write(t, dir, "untracked.txt", "loose\n")

	counts, err := Read(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Branch != "main" {
		t.Errorf("branch = %q, want main", counts.Branch)
	}
	if counts.Dirty == 0 {
		t.Error("dirty = 0, want the modified file counted")
	}
	if counts.Staged == 0 {
		t.Error("staged = 0, want the added file counted")
	}
	if counts.Untracked == 0 {
		t.Error("untracked = 0, want the loose file counted")
	}
	// The gap that made this package necessary: porcelain v2 has no
	// stash field at all, so a scan that only parses it reports zero
	// however many stashes exist.
	if counts.Stashed != 1 {
		t.Errorf("stashed = %d, want 1", counts.Stashed)
	}
}

// A conflict is counted both as conflicted and as dirty: p10k gives the
// state its own color, and a tree mid-merge is not clean either.
func TestConflictedIsCountedTwice(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "f.txt", "base\n")
	git(t, dir, "add", "f.txt")
	git(t, dir, "commit", "-qm", "base")

	git(t, dir, "checkout", "-q", "-b", "other")
	write(t, dir, "f.txt", "theirs\n")
	git(t, dir, "commit", "-qam", "theirs")
	git(t, dir, "checkout", "-q", "main")
	write(t, dir, "f.txt", "ours\n")
	git(t, dir, "commit", "-qam", "ours")

	// The merge is expected to fail; that is the point. It still needs
	// the identity env the helper sets — without it the first version
	// failed for the wrong reason and left a clean tree, which read as
	// "conflicts are not counted".
	gitAllowFail(t, dir, "merge", "other")

	counts, err := Read(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Conflicted == 0 {
		t.Errorf("conflicted = 0 during a conflicted merge: %+v", counts)
	}
	if counts.Dirty == 0 {
		t.Errorf("dirty = 0 during a conflicted merge: %+v", counts)
	}
}

// A clean tree reports nothing, so a prompt does not decorate a
// repository that has no news.
func TestCleanTreeCountsZero(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "f.txt", "x\n")
	git(t, dir, "add", "f.txt")
	git(t, dir, "commit", "-qm", "only")

	counts, err := Read(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Dirty+counts.Staged+counts.Untracked+counts.Conflicted+counts.Stashed != 0 {
		t.Errorf("clean tree reported %+v", counts)
	}
}
