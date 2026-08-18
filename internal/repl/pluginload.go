package repl

import (
	"context"
	"fmt"
	"os"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/manifest"
	"github.com/blairham/koi-shell/internal/plugmgr"
)

// loadPluginManifest brings up the declarative plugin surface (#108):
// read the manifest, install and source what is enabled and eager, and
// register lazy triggers. Errors are reported — a plugin list that
// silently does nothing is worse than one that complains.
func loadPluginManifest(ctx context.Context, runner *interp.Runner) {
	mgr, err := plugmgr.NewZi(os.Stderr)
	if err != nil {
		return // no manager, no manifest; zi's own path reports this
	}
	pluginMgr, err = newPluginManager(mgr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "koi: plugins:", err)
		pluginMgr = nil
		return
	}
	for _, payload := range pluginMgr.loadEager() {
		quoted, qerr := syntax.Quote(payload, syntax.LangBash)
		if qerr != nil {
			continue
		}
		if rerr := runEnvScript(ctx, runner, "source "+quoted+"\n"); rerr != nil {
			fmt.Fprintf(os.Stderr, "koi: plugin %s: %v\n", payload, rerr)
		}
	}
}

// ziDeprecationNotice is printed once per session the first time a `zi`
// command runs: the ice-modifier surface still works, but the
// documented way to manage plugins is the manifest (#108).
func ziDeprecationNotice(w interp.HandlerContext) {
	if ziNoticeShown {
		return
	}
	ziNoticeShown = true
	fmt.Fprintln(w.Stderr, strings.TrimSpace(`
koi: zi still works, but `+"`plugin`"+` is the supported surface now — four
      knobs in one file instead of ice modifiers to memorize. See
      `+"`plugin help`"+`; `+"`zi migrate`"+` converts what you already have.`))
}

var ziNoticeShown bool

// ziMigrate converts what zi already installed into manifest entries —
// the one-command migration promised by #108, so existing users move
// without retyping their setup.
func ziMigrate(mgr plugmgr.Manager, hc interp.HandlerContext) []string {
	lister, ok := mgr.(plugmgr.ObjectLister)
	if !ok || pluginMgr == nil {
		fmt.Fprintln(hc.Stderr, "zi: migration is unavailable in this session")
		return []string{"false"}
	}
	objects, err := lister.Objects()
	if err != nil {
		fmt.Fprintln(hc.Stderr, "zi:", err)
		return []string{"false"}
	}
	added, kept := 0, 0
	for _, o := range objects {
		entry := manifest.Plugin{Source: o.Raw}
		if o.Kind == "snippet" {
			entry.Kind = manifest.KindSnippet
		} else if strings.Contains(o.Ices, "gh-r") {
			entry.Kind = manifest.KindRelease
		}
		if pluginMgr.man.Find(entry.Source) >= 0 {
			kept++
			continue
		}
		pluginMgr.man.Add(entry)
		added++
	}
	if added == 0 {
		fmt.Fprintf(hc.Stdout, "nothing to migrate (%d already in the manifest)\n", kept)
		return []string{"true"}
	}
	if err := pluginMgr.man.Save(pluginMgr.path); err != nil {
		fmt.Fprintln(hc.Stderr, "zi:", err)
		return []string{"false"}
	}
	fmt.Fprintf(hc.Stdout, "migrated %d plugin(s) into %s — review it, then drop the zi lines from your rc\n",
		added, displayPath(pluginMgr.path))
	fmt.Fprintln(hc.Stdout, "ices that did not survive the translation (wait, pick, atload…) need a hand check")
	return []string{"true"}
}
