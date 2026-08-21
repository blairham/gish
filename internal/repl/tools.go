package repl

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/tools"
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
	// lastRes is the most recent resolution, so session recording (#103)
	// can report which pins are actually in effect without re-walking
	// the tree at every prompt.
	lastRes tools.Resolution
}

func newToolsManager(notices io.Writer) *toolsManager {
	return &toolsManager{notices: notices, warned: map[string]bool{}}
}

// invalidate forces the next prompt to re-resolve (pins or installs
// changed under the same directory).
func (t *toolsManager) invalidate() { t.lastDir = "" }

// atPrompt rebuilds PATH when the directory (or the KOI_TOOLS switch)
// changed what should be active.
func (t *toolsManager) atPrompt(ctx context.Context, runner *interp.Runner) {
	if shellVar(runner, "KOI_TOOLS", "on") == "off" {
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
	t.lastRes = res
	for _, pin := range res.Missing {
		key := res.File + "\x00" + pin.Tool
		if t.warned[key] {
			continue
		}
		t.warned[key] = true
		fmt.Fprintf(t.notices, "koi: tools: %s pins %s %s but it is not installed (asdf install %s %s)\n",
			displayPath(res.File), pin.Tool, pin.Versions[0], pin.Tool, pin.Versions[0])
	}
	t.setPrepends(ctx, runner, res.Bins)
}

// activePins reports the pins in effect: everything the nearest
// .tool-versions asks for, minus the ones with no installed version.
// A pin that cannot resolve is not "active" — recording it would tell a
// restored session it had a toolchain it never had.
func (t *toolsManager) activePins() map[string]string {
	if t == nil || t.lastRes.File == "" {
		return nil
	}
	missing := make(map[string]bool, len(t.lastRes.Missing))
	for _, pin := range t.lastRes.Missing {
		missing[pin.Tool] = true
	}
	out := map[string]string{}
	for _, pin := range tools.ParseFile(t.lastRes.File) {
		if missing[pin.Tool] || len(pin.Versions) == 0 {
			continue
		}
		out[pin.Tool] = pin.Versions[0]
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
