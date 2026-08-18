package repl

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/dirjump"
	"github.com/blairham/koi-shell/internal/history"
)

// Native z (#94): frecency jumping with the shell as the tracking
// point. The loop notes every directory change (no prompt hooks to
// wire), a fresh index bootstraps from the history store's cwds, and
// ambiguous queries open the #90 chooser instead of requiring fzf.

// jumpMgr is set at interactive startup; nil disables the feature.
var jumpMgr *jumpManager

type jumpManager struct {
	store   *dirjump.Store
	lastDir string
}

// newJumpManager opens the index and, when empty, seeds it from the
// history store — day one already knows where the user works.
func newJumpManager(store *history.Store) *jumpManager {
	path, err := dirjump.DefaultPath()
	if err != nil {
		return nil
	}
	index, err := dirjump.Open(path)
	if err != nil {
		return nil
	}
	if index.Empty() && store != nil {
		index.Seed(store.DirCounts(), time.Now().Add(-48*time.Hour))
	}
	return &jumpManager{store: index}
}

// note records a directory change at prompt time; consecutive prompts
// in the same directory count once.
func (j *jumpManager) note(runner *interp.Runner) {
	if shellVar(runner, "KOI_JUMP", "on") == "off" {
		return
	}
	dir := runner.Dir
	if dir == j.lastDir {
		return
	}
	j.lastDir = dir
	j.store.Visit(dir, time.Now())
}

// save persists the index on shell exit.
func (j *jumpManager) save() {
	_ = j.store.Save(time.Now()) //nolint:errcheck // derived state, best-effort
}

const zUsage = `usage: z [-i | -l] [terms…]

  z api          jump to the best frecency match for "api"
  z proj api     terms match in path order; the last names the target dir
  z              pick from your top directories
  z -i api       force the picker on a query
  z -l [terms]   list matches with their scores`

// zCallHandler intercepts `z`, config-style: the rewrite hands the
// interpreter a plain cd, so the session moves for real.
func zCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "z" {
			return next(ctx, args)
		}
		return runZ(interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runZ(hc interp.HandlerContext, args []string) []string {
	fail := func(err error) []string {
		fmt.Fprintln(hc.Stderr, "z:", err)
		return []string{"false"}
	}
	if jumpMgr == nil {
		// Non-interactive sessions still answer queries; tracking stays
		// interactive-only (zoxide-consistent — scripts and CI must not
		// pollute the index).
		jumpMgr = newJumpManager(nil)
		if jumpMgr == nil {
			return fail(fmt.Errorf("directory index unavailable"))
		}
	}
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(hc.Stdout, zUsage)
		return []string{"true"}
	}

	list, pick := false, false
	for len(args) > 0 {
		switch args[0] {
		case "-l":
			list, args = true, args[1:]
		case "-i":
			pick, args = true, args[1:]
		default:
			goto terms
		}
	}
terms:
	matches := jumpMgr.store.Query(args, time.Now())
	// The current directory is never a jump target.
	matches = slices.DeleteFunc(matches, func(m dirjump.Match) bool { return m.Dir == hc.Dir })
	if len(matches) == 0 {
		return fail(fmt.Errorf("no match for %q", strings.Join(args, " ")))
	}

	switch {
	case list:
		for _, m := range matches {
			fmt.Fprintf(hc.Stdout, "%8.1f  %s\n", m.Score, m.Dir)
		}
		return []string{"true"}
	case pick || len(args) == 0:
		return zPick(hc, matches, fail)
	default:
		return cdTo(matches[0].Dir, fail)
	}
}

// zPick opens the chooser over the top matches; without a terminal it
// degrades to the ranked list.
func zPick(hc interp.HandlerContext, matches []dirjump.Match, fail func(error) []string) []string {
	choose := interactiveChooser(hc.Stdin, hc.Stdout)
	if choose == nil {
		for _, m := range matches[:min(len(matches), 15)] {
			fmt.Fprintf(hc.Stdout, "%8.1f  %s\n", m.Score, m.Dir)
		}
		return []string{"true"}
	}
	options := make([]chooseOption, 0, min(len(matches), 15))
	for _, m := range matches[:min(len(matches), 15)] {
		options = append(options, chooseOption{key: m.Dir, label: displayPath(m.Dir)})
	}
	dir, ok := choose("jump where?", options)
	if !ok {
		return []string{"true"}
	}
	return cdTo(dir, fail)
}

func cdTo(dir string, fail func(error) []string) []string {
	quoted, err := syntax.Quote(dir, syntax.LangBash)
	if err != nil {
		return fail(err)
	}
	return []string{"eval", "cd " + quoted}
}
