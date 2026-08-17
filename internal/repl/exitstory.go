package repl

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/history"
	"github.com/blairham/gish/internal/term"
	"github.com/blairham/gish/internal/ui"
)

// The exit story (#212). The forgotten half of every fast-adoption story
// is exit cost: uv's skeptics never blocked adoption because rollback was
// free, and ripgrep could refuse grep compatibility outright because
// quitting cost nothing. Inverted, exit cost is the first-ten-minutes
// churn stage — the documented fish lockout stories are all people who
// ran chsh and then could not get back.
//
// gish already makes the claim true: startup creates nothing (#163), the
// login shell is never touched, and no dotfile is rewritten. What was
// missing is *saying* it, once, where a trial user is standing.
//
// The obvious implementation — write a "already said this" marker on
// first run — was built first and TestStartupWritesNothingToHome
// rejected it, correctly. A notice that claims nothing on the machine was
// changed, and creates a file in order to claim it, is false at the
// moment it is printed. #163's rule is not in tension with this feature;
// it *is* this feature.
//
// So nothing is recorded. The notice is shown while gish has left no
// trace yet, which is exactly the window where the claim is true and the
// reader has invested nothing. Running one command creates history, and
// the notice never appears again. Someone who opens three tabs and types
// nothing sees it three times — they are three seconds into their first
// session, which is precisely the audience it is for.

// exitStory prints the revert instructions on a first run. It runs after
// the rc file so `config welcome off` is honored on the very first
// session — the one that matters when a new machine is bootstrapped from
// dotfiles.
func exitStory(runner *interp.Runner) {
	if shellVar(runner, "GISH_WELCOME", "") == "off" {
		return
	}
	// A trial user is at a terminal. Piped or scripted invocations get
	// nothing: this is a conversation, not output.
	if !term.IsTerminal(os.Stdout) {
		return
	}
	writeExitStory(os.Stderr, untouched())
}

// untouched reports whether gish has left anything on this machine yet.
// It only ever stats; it never creates, which is what lets the notice it
// gates stay true.
//
// The signal is the gish *directories*, not the history file
// specifically. Keying on history.jsonl was tried and was wrong: a
// session can leave the directory containing z's dirs.json without ever
// flushing a history entry, so a user who had plainly used the shell
// still read as a first look. Anything under these roots means gish has
// been here.
func untouched() bool {
	for _, dir := range gishStateRoots() {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(dir); err == nil {
			return false
		}
	}
	// An rc means configured, which means not a first look — even on a new
	// machine with no data yet, which is the dotfiles-restore case.
	if path := rcPath(); path != "" {
		if _, err := os.Stat(path); err == nil {
			return false
		}
	}
	return true
}

// gishStateRoots are the per-user directories gish writes into, derived
// from the same paths the writers use rather than rebuilt by hand.
func gishStateRoots() []string {
	var roots []string
	if path, err := history.DefaultPath(); err == nil && path != "" {
		roots = append(roots, filepath.Dir(path)) // $XDG_DATA_HOME/gish
	}
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		roots = append(roots, filepath.Join(stateHome, "gish"))
	} else if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".local", "state", "gish"))
	}
	return roots
}

// writeExitStory is the part with behavior in it, split out so tests can
// reach it: the TTY and setting gates above are environment, and a
// function only reachable through a real terminal is a function nothing
// ever asserts on.
func writeExitStory(w io.Writer, firstLook bool) {
	if !firstLook {
		return
	}
	style := ui.Styles(ui.Enabled(w))
	fmt.Fprintln(w, style.Dim.Render(
		"gish: nothing on this machine was changed — not your login shell, no dotfiles rewritten. "+
			revertCommand()))
	fmt.Fprintln(w, style.Dim.Render(
		"      `config welcome off` skips this on machines you set up from dotfiles."))
}

// revertCommand names the actual way out for this install, because
// "uninstall it" is only reassuring when it is a command you can read.
// Homebrew is detected the way #44 detects it — stat and string work, no
// subprocess and no network on the startup path.
func revertCommand() string {
	self, err := os.Executable()
	if err != nil {
		return "Uninstalling removes it completely; gish leaves nothing else behind."
	}
	if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = resolved
	}
	for _, prefix := range brewPrefixes {
		// Cellar is the real install root; the bin entry is a symlink into
		// it, which is why the path is resolved first.
		if strings.HasPrefix(self, filepath.Join(prefix, "Cellar")+string(filepath.Separator)) {
			return "Undo the whole thing with `brew uninstall gish`."
		}
	}
	return fmt.Sprintf("Undo the whole thing by deleting %s.", self)
}
