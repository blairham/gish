package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Cloning and updating plugins runs against a local fixture repo: the
// operations are real git, the network is not involved. Every commit
// here needs an identity and must ignore the developer's own git config,
// or the results depend on whose machine ran them.

func gitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "gitconfig-system"))
	t.Setenv("GIT_AUTHOR_NAME", "gish test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "gish test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// fixtureRepo builds a repo with two commits and returns its path.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	dir := t.TempDir()
	git(t, dir, "init", "--initial-branch=main", ".")
	if err := os.WriteFile(filepath.Join(dir, "plugin.sh"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "first")
	git(t, dir, "tag", "v1")
	if err := os.WriteFile(filepath.Join(dir, "plugin.sh"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-am", "second")
	return dir
}

func TestCloneCheckoutAndLog(t *testing.T) {
	gitEnv(t)
	origin := fixtureRepo(t)

	t.Run("head", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "clone")
		if err := Clone(origin, dest, "", 0); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dest, "plugin.sh"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "second\n" {
			t.Errorf("cloned content = %q, want the tip", got)
		}
		// Log is what `zi` shows for an object's recent history.
		out, err := Log(dest, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "second") {
			t.Errorf("Log(1) = %q, want the newest commit", out)
		}
	})

	t.Run("pinned version", func(t *testing.T) {
		// The ver ice pins a tag; the checkout happens after the clone.
		dest := filepath.Join(t.TempDir(), "clone")
		if err := Clone(origin, dest, "v1", 0); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dest, "plugin.sh"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "first\n" {
			t.Errorf("pinned content = %q, want the tagged commit", got)
		}
	})

	t.Run("shallow", func(t *testing.T) {
		// file:// rather than a bare path: git ignores --depth for local
		// path clones (it hardlinks the object store instead), so a plain
		// path would fetch everything and the assertion would be about
		// git's local optimization rather than about the flag gish built.
		dest := filepath.Join(t.TempDir(), "clone")
		if err := Clone("file://"+origin, dest, "", 1); err != nil {
			t.Fatal(err)
		}
		if n := strings.TrimSpace(git(t, dest, "rev-list", "--count", "HEAD")); n != "1" {
			t.Errorf("depth 1 cloned %s commits, want 1", n)
		}
	})
}

func TestCloneReportsFailure(t *testing.T) {
	gitEnv(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	// A path that is not a repo: the error must carry git's own words,
	// since that is what the user is shown.
	err := Clone(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "dest"), "", 0)
	if err == nil {
		t.Fatal("cloning a non-repository succeeded")
	}
	if !strings.Contains(err.Error(), "git clone") {
		t.Errorf("error does not name the failing command: %v", err)
	}
}

func TestPullReportsWhetherAnythingArrived(t *testing.T) {
	gitEnv(t)
	origin := fixtureRepo(t)

	dest := filepath.Join(t.TempDir(), "clone")
	if err := Clone(origin, dest, "", 0); err != nil {
		t.Fatal(err)
	}

	// Nothing new upstream: no change, and Update must not rerun hooks.
	changed, err := Pull(dest)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("Pull reported a change with nothing new upstream")
	}

	// A new commit upstream: changed, so hooks should rerun.
	if err := os.WriteFile(filepath.Join(origin, "plugin.sh"), []byte("third\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "commit", "-am", "third")

	changed, err = Pull(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("Pull missed a new upstream commit")
	}
}

// IsRepo decides whether Update tries to pull at all. It requires .git to
// be a directory, so a worktree or submodule checkout — where .git is a
// *file* — reports false and the object is silently skipped. That is
// worth pinning: it is a plausible layout, and the failure is quiet.
func TestIsRepo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if IsRepo(dir) {
		t.Error("a plain directory reported as a repository")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsRepo(dir) {
		t.Error("a directory with .git/ was not recognized")
	}

	fileGit := t.TempDir()
	if err := os.WriteFile(filepath.Join(fileGit, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsRepo(fileGit) {
		t.Error("a .git file (worktree/submodule layout) is now recognized — " +
			"if that is deliberate, update installer.Update's skip branch too")
	}
}
