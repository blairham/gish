package repl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/history"
)

// The notice belongs to the window where its own claim is true, and must
// stop once koi has left a trace — otherwise it is both nagging and, by
// then, lying.
func TestExitStoryOnlyWhileNothingHasBeenLeftBehind(t *testing.T) {
	var first bytes.Buffer
	writeExitStory(&first, true)
	if first.Len() == 0 {
		t.Fatal("first look printed nothing; the trial user never learns the way out")
	}

	var later bytes.Buffer
	writeExitStory(&later, false)
	if later.Len() != 0 {
		t.Errorf("printed after koi had left state behind: %q", later.String())
	}
}

// untouched drives that decision, and it must read state rather than
// create it — the whole point is that looking does not count as using.
func TestUntouchedSeesTheFirstCommandAndCreatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("KOI_RC", filepath.Join(home, "cfg", "koi", "koirc"))

	if !untouched() {
		t.Fatal("a fresh home did not read as untouched")
	}
	if entries, err := os.ReadDir(home); err == nil && len(entries) != 0 {
		t.Errorf("the check itself created %d entries in $HOME", len(entries))
	}

	// The data directory is the signal, not history.jsonl specifically: a
	// session can leave z's dirs.json there without flushing a history
	// entry, and that user has plainly used the shell.
	path, err := history.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Dir(path)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "dirs.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if untouched() {
		t.Error("still untouched after a session left files in the data directory")
	}
}

// The state directory counts too — session records and the command index
// land there, and either means koi has run here before.
func TestUntouchedSeesTheStateDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("KOI_RC", filepath.Join(home, "koirc"))

	if !untouched() {
		t.Fatal("a fresh home did not read as untouched")
	}
	if err := os.MkdirAll(filepath.Join(home, "state", "koi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if untouched() {
		t.Error("still untouched with a koi state directory present")
	}
}

// A configured user is not a first-time user, even with no history —
// someone restoring dotfiles onto a new machine has an rc and does not
// need to be told how to uninstall.
func TestUntouchedRespectsAnExistingRC(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	rc := filepath.Join(home, "koirc")
	t.Setenv("KOI_RC", rc)
	if err := os.WriteFile(rc, []byte("KOI_THEME=p10k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if untouched() {
		t.Error("an existing rc file still read as a first look")
	}
}

// The reassurance is the content, so assert on it rather than on the fact
// that bytes were produced — a notice that stopped saying "nothing was
// changed" would still pass a length check.
func TestExitStorySaysNothingWasChangedAndHowToUndo(t *testing.T) {
	var out bytes.Buffer
	writeExitStory(&out, true)
	got := out.String()

	for _, want := range []string{"nothing on this machine was changed", "not your login shell", "Undo"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice does not mention %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "chsh -s") {
		t.Errorf("the first-run notice should not teach chsh — that is the commitment we exist to defer:\n%s", got)
	}
}

// The revert line has to name a command the reader can actually run;
// "uninstall it somehow" is not an exit story.
func TestRevertCommandNamesSomethingRunnable(t *testing.T) {
	got := revertCommand()
	if !strings.Contains(got, "brew uninstall koi") && !strings.Contains(got, "deleting ") {
		t.Errorf("revert command names no concrete action: %q", got)
	}
}

// checkLoginShell's happy path is the reversibility claim itself: if koi
// is not $SHELL, doctor must say there is nothing to undo.
func TestDoctorLoginShellReportsNothingToRevert(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	got := checkLoginShell()
	if got.status != checkOK {
		t.Fatalf("status = %v, want OK when koi is not the login shell (detail: %q)", got.status, got.detail)
	}
	if !strings.Contains(got.detail, "nothing to revert") {
		t.Errorf("detail does not state the reversibility claim: %q", got.detail)
	}
}

func TestFallbackShellPrefersAListedConventionalShell(t *testing.T) {
	if got := fallbackShell([]string{"/usr/bin/fish", "/bin/bash", "/bin/zsh"}); got != "/bin/zsh" {
		t.Errorf("fallbackShell = %q, want /bin/zsh (preferred over bash and fish)", got)
	}
	// Never name a shell that is not there: the revert command has to work.
	if got := fallbackShell(nil); got != "/bin/sh" {
		t.Errorf("fallbackShell(nil) = %q, want the POSIX fallback /bin/sh", got)
	}
}

func TestSameFileFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "koi")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "koi-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symlink support: %v", err)
	}
	// /etc/shells lists /bin/zsh while os.Executable resolves into a
	// Cellar path; comparing strings would report "not listed" and send
	// the user to add a duplicate line.
	if !sameFile(link, real) {
		t.Error("sameFile did not see through the symlink")
	}
	if sameFile(real, filepath.Join(dir, "absent")) {
		t.Error("sameFile matched a path that does not exist")
	}
}
