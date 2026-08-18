package repl

import (
	"context"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// `set -o vi` (#163).
//
// The line is in a great many inherited rc files, and it is the first
// thing a vi-mode user types when a new shell feels wrong. Left alone,
// the interpreter answers `set: invalid option: "vi"` and carries on —
// which is the `alias` trap again: the shell says something went wrong
// in a way that reads like a broken rc, while the setting the user asked
// for silently does not exist.
//
// Rewritten to the assignment that already drives the editor, so there
// is one source of truth for the mode and `set -o vi`, `config editmode
// vi`, and KOI_EDIT_MODE=vi are the same switch reached three ways.
// Only the four exact forms are claimed; every other `set` invocation
// belongs to the interpreter and is passed through untouched.
func setOptionCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if len(args) == 3 && args[0] == "set" && (args[1] == "-o" || args[1] == "+o") {
			on := args[1] == "-o"
			switch args[2] {
			case "vi":
				return editModeAssign(on), nil
			case "emacs":
				return editModeAssign(!on), nil
			}
		}
		return next(ctx, args)
	}
}

func editModeAssign(vi bool) []string {
	mode := "emacs"
	if vi {
		mode = "vi"
	}
	return []string{"eval", "KOI_EDIT_MODE=" + mode}
}
