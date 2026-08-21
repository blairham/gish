package repl

import (
	"strings"

	"github.com/blairham/koi-shell/internal/editor"
	"github.com/blairham/koi-shell/internal/shell/interp"
)

// `set -o vi` (#163, then #576).
//
// The line is in a great many inherited rc files, and it is the first
// thing a vi-mode user types when a new shell feels wrong. Left alone,
// the interpreter answered `set: invalid option: "vi"` and carried on —
// which is the `alias` trap again: the shell says something went wrong
// in a way that reads like a broken rc, while the setting the user asked
// for silently does not exist.
//
// #163 answered that by *rewriting* the four exact forms to the
// KOI_EDIT_MODE assignment that drives the editor. The editor switched,
// which is what the user asked for, and because the rewrite replaced the
// command the interpreter never saw the `set` — so `set -o`, `shopt -o
// vi` and `$SHELLOPTS` all reported emacs in a shell that was editing in
// vi, and a script saving and restoring the mode restored the wrong one
// (#576). That is #566's lesson again: a layer that answers a *request*
// on behalf of another must not leave that layer's own answer wrong.
//
// The interpreter owns the bit now, so `set -o vi` is passed straight
// through and there is nothing left to intercept: `emacs` and `vi` are
// ordinary supported options there, mutually exclusive as bash's are, and
// [editModeOf] reads the option rather than a variable of koi's own. What
// remains here is the other direction — KOI_EDIT_MODE and `config
// editmode` are koi's own spelling of the same switch, and the option has
// to report what they asked for too.

// editModeVar is the variable `config editmode` writes and an rc or the
// environment sets. It is a *request*: the option bits are the state, and
// [applyEditModeVar] resolves one into the other.
const editModeVar = "KOI_EDIT_MODE"

// editModeOf resolves the line editor's dialect (#163).
//
// vi mode is a documented abandonment cause in both directions: its
// absence drove people back to zsh, and NO_COLOR-style all-or-nothing
// switches are not how anyone expects to reach it. It is reached three
// ways — `set -o vi`, `config editmode vi`, KOI_EDIT_MODE — and all three
// now arrive at one place, the interpreter's own option bit, so the shell
// cannot be editing in one mode while `set -o` reports the other (#576).
//
// Neither bit set is emacs, which is bash's answer too: a non-interactive
// shell has both off and readline with no mode asked for is emacs, so
// `set -o vi; set +o vi` leaves the editor where it started rather than
// in a third state.
func editModeOf(runner *interp.Runner) editor.EditMode {
	if runner.OptionSet("vi") {
		return editor.ModeVi
	}
	return editor.ModeEmacs
}

// applyEditModeVar moves the `set -o` bit to whatever KOI_EDIT_MODE says,
// and reports the value it acted on.
//
// The caller keeps that value and hands it back next time, because the
// variable can only be allowed to speak when it *changes*. Reading it
// unconditionally would make it the source of truth and undo every
// `set -o vi` at the following prompt, since an unset variable means
// emacs; comparing it against the bit instead would fight in the other
// direction, re-asserting a stale rc setting the moment the option moved.
// So the rule is the one `config` already follows: a setting takes effect
// when it is set. The corner that leaves is worth stating — assigning
// KOI_EDIT_MODE the value it already had, after a `set -o` moved the bit
// the other way, is not a change and does nothing.
func applyEditModeVar(runner *interp.Runner, seen string) string {
	value := shellVar(runner, editModeVar, "")
	if value == seen {
		return seen
	}
	switch strings.ToLower(value) {
	case "vi":
		_ = interp.Params("-o", "vi")(runner)
	case "emacs":
		_ = interp.Params("-o", "emacs")(runner)
	}
	return value
}
