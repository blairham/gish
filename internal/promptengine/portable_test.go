package promptengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The path-shaped corners this package has, pinned so the Windows job
// can run it (#136).
//
// The code here was already portable — filepath everywhere, a
// writable_windows.go — but nothing exercised it on Windows, and the
// tests themselves assumed a unix layout. That is the same class of gap
// #47's first slice closed elsewhere: a prompt that renders a mangled
// path is exactly the kind of thing nobody notices until someone runs
// it.

// tildify is what turns a home directory into `~`, and it compares
// against the platform separator — which is the specific reason the
// render fixtures could not be slash-separated literals.
func TestTildifyIsNative(t *testing.T) {
	t.Parallel()

	home := filepath.Join(string(filepath.Separator), "fixture", "you")
	sep := string(filepath.Separator)

	if got := tildify(home, home); got != "~" {
		t.Errorf("home itself = %q, want ~", got)
	}
	if got, want := tildify(filepath.Join(home, "dev"), home), "~"+sep+"dev"; got != want {
		t.Errorf("under home = %q, want %q", got, want)
	}
	// A sibling whose name merely starts with the home path must not be
	// abbreviated: `/fixture/youtube` is not inside `/fixture/you`.
	sibling := filepath.Join(string(filepath.Separator), "fixture", "youtube")
	if got := tildify(sibling, home); got != sibling {
		t.Errorf("sibling directory = %q, want it left alone", got)
	}
	if got := tildify(sibling, ""); got != sibling {
		t.Errorf("empty home = %q, want the path unchanged", got)
	}
}

// The walk up must terminate at the volume root rather than spinning:
// filepath.Dir("C:\\") returns "C:\\", exactly as Dir("/") returns "/",
// and the loop's exit depends on noticing that.
func TestFindGitDirTerminatesAtTheRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir() // a real, git-less directory on this platform
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, ok := findGitDir(dir); ok {
			t.Error("found a repository where there is none")
		}
	}()
	<-done // a non-terminating walk fails as a test timeout, which is the honest symptom
}

// The `.git` *file* case — worktrees and submodules — is entirely
// path-shaped: a relative pointer is joined against the directory that
// held the file.
func TestFindGitDirFollowsAGitFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	work := filepath.Join(root, "work")
	real := filepath.Join(root, "store", "worktrees", "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("absolute pointer", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+real+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitDir, workTree, ok := findGitDir(work)
		if !ok || gitDir != real || workTree != work {
			t.Errorf("gitDir=%q workTree=%q ok=%v, want %q and %q", gitDir, workTree, ok, real, work)
		}
	})

	t.Run("relative pointer", func(t *testing.T) {
		rel := filepath.Join("..", "store", "worktrees", "work")
		if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+rel+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitDir, _, ok := findGitDir(work)
		if !ok {
			t.Fatal("relative pointer not followed")
		}
		if got, want := filepath.Clean(gitDir), filepath.Clean(real); got != want {
			t.Errorf("gitDir = %q, want %q", got, want)
		}
	})
}

// The config path honors XDG_CONFIG_HOME and otherwise falls back to
// the user's home — which on Windows means USERPROFILE, and is the
// fallback nothing pinned.
func TestConfigPathFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads this one on Windows

	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "koi", ConfigFileName); path != want {
		t.Errorf("ConfigPath = %q, want %q", path, want)
	}

	explicit := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", explicit)
	path, err = ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(explicit, "koi", ConfigFileName); path != want {
		t.Errorf("ConfigPath with XDG = %q, want %q", path, want)
	}
}

// A double-width icon must not silently push the frame past the
// terminal's width: the gap arithmetic is what keeps a powerline ribbon
// from wrapping, and it is measured in columns, not runes.
func TestWideIconsAreMeasuredInColumns(t *testing.T) {
	t.Parallel()

	ctx := sampleContext()
	ctx.Width = 40
	cfg := Preset("lean")
	// A nerd-font glyph that is two columns wide in most fonts.
	cfg.Set("DIR_VISUAL_IDENTIFIER_EXPANSION", "📁")

	got := Render(cfg, ctx)
	for _, line := range strings.Split(plain(got.Prompt), "\n") {
		if w := displayWidth(line); w > ctx.Width {
			t.Errorf("line is %d columns wide, past the %d available: %q", w, ctx.Width, line)
		}
	}
}
