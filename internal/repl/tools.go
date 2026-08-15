package repl

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/tools"
)

// Native tool-version switching (#77 v1): on directory change the pins
// in scope resolve against the asdf install tree and PATH is rebuilt —
// resolved bin dirs prepend, the previous prepends drop out, and
// everything else in PATH is preserved, so a user's own mid-session
// PATH edits survive. First-party env work: it runs before EnvProvider
// plugins and needs no trust prompt (pins can only select among
// already-installed versions; see internal/tools).
// toolsMgr is set at interactive startup; the tool builtin pokes it
// to force a re-resolve after pin edits and installs.
var toolsMgr *toolsManager

type toolsManager struct {
	notices io.Writer

	lastDir string
	applied []string        // our current PATH prepends
	warned  map[string]bool // file+tool notices already printed
}

func newToolsManager(notices io.Writer) *toolsManager {
	return &toolsManager{notices: notices, warned: map[string]bool{}}
}

// invalidate forces the next prompt to re-resolve (pins or installs
// changed under the same directory).
func (t *toolsManager) invalidate() { t.lastDir = "" }

// atPrompt rebuilds PATH when the directory (or the GISH_TOOLS switch)
// changed what should be active.
func (t *toolsManager) atPrompt(ctx context.Context, runner *interp.Runner) {
	if shellVar(runner, "GISH_TOOLS", "on") == "off" {
		t.lastDir = "" // re-resolve when re-enabled
		t.setPrepends(ctx, runner, nil)
		return
	}
	dir := runner.Dir
	if dir == t.lastDir {
		return
	}
	t.lastDir = dir

	res := tools.Resolve(dir, tools.InstallRoots())
	for _, pin := range res.Missing {
		key := res.File + "\x00" + pin.Tool
		if t.warned[key] {
			continue
		}
		t.warned[key] = true
		fmt.Fprintf(t.notices, "gish: tools: %s pins %s %s but it is not installed (asdf install %s %s)\n",
			displayPath(res.File), pin.Tool, pin.Versions[0], pin.Tool, pin.Versions[0])
	}
	t.setPrepends(ctx, runner, res.Bins)
}

// setPrepends swaps our prepends at the front of PATH, keeping every
// entry we did not add.
func (t *toolsManager) setPrepends(ctx context.Context, runner *interp.Runner, bins []string) {
	if slices.Equal(bins, t.applied) {
		return
	}
	var current string
	if v, ok := runner.Vars["PATH"]; ok {
		current = v.String()
	}
	kept := make([]string, 0, 8)
	for _, entry := range filepath.SplitList(current) {
		if !slices.Contains(t.applied, entry) {
			kept = append(kept, entry)
		}
	}
	newPath := strings.Join(append(slices.Clone(bins), kept...), string(os.PathListSeparator))
	quoted, err := syntax.Quote(newPath, syntax.LangBash)
	if err != nil {
		return
	}
	if runEnvScript(ctx, runner, "export PATH="+quoted+"\n") != nil {
		return
	}
	t.applied = slices.Clone(bins)
}
