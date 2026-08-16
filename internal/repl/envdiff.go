package repl

import (
	"context"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/envtrust"
	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

// Env diffs (#12): on directory change an EnvProvider plugin proposes
// environment changes, direnv-style. The trust model is enforced here,
// host-side:
//
//   - Nothing applies silently. An untrusted proposal pends with a
//     one-line notice; `trust allow` records (plugin, dir, diff-hash)
//     and applies. A changed diff re-pends — edit-reprompts semantics.
//   - Deny-listed variables (loader hooks, IFS, shell internals) are
//     stripped before a proposal is even considered.
//   - Requests carry the allowlisted env subset, never the full
//     environment.
//
// Applied diffs revert when the shell leaves the proposal's subtree:
// the manager snapshots every touched variable and restores it.

// envMgr is set at interactive startup when a plugin host and a
// readable trust store exist. Package-level like themePlugins: the
// prompt loop and the trust builtin share it.
var envMgr *envManager

// envDenied reports variables no plugin may ever set or unset:
// process-loader hooks, word-splitting, startup-file redirection, and
// gish's own knobs. PATH is deliberately settable — but only ever
// through the visible allow flow.
func envDenied(name string) bool {
	switch name {
	case "IFS", "ENV", "BASH_ENV", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT":
		return true
	}
	return strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "GISH_")
}

// envProposal is one stripped, validated, hashed proposal.
type envProposal struct {
	plugin, forDir string
	set            map[string]string
	unset          []string
	stripped       []string // deny-listed names the host removed
	hash           string
}

// appliedEnv tracks the live diff so it can be reverted.
type appliedEnv struct {
	plugin, forDir, hash string
	saved                map[string]*string // previous value; nil = was unset
}

type envManager struct {
	trust   *envtrust.Store
	notices io.Writer
	nextSeq func() uint64

	warmed    chan struct{}
	providers []pluginhost.Provider[pluginapi.EnvProviderClient]

	mu       sync.Mutex
	lastDir  string
	pending  *envProposal
	active   *appliedEnv
	notified string // forDir+hash already announced — a changed diff re-notifies
}

func newEnvManager(host *pluginhost.Host, trust *envtrust.Store, notices io.Writer) *envManager {
	m := &envManager{
		trust:   trust,
		notices: notices,
		nextSeq: host.NextSeq,
		warmed:  make(chan struct{}),
	}
	go func() {
		defer close(m.warmed)
		ctx, cancel := context.WithTimeout(context.Background(), pluginhost.DescribeTimeout)
		defer cancel()
		m.providers = host.EnvProviders(ctx)
	}()
	return m
}

// atPrompt runs at prompt time. It reverts the active diff when the
// shell has left its subtree, and on directory change asks providers
// for a proposal — applying it silently only when this exact diff was
// allowed before, pending it with a notice otherwise.
func (m *envManager) atPrompt(ctx context.Context, runner *interp.Runner) {
	dir := runner.Dir

	m.mu.Lock()
	if m.active != nil && !underDir(dir, m.active.forDir) {
		m.revertLocked(ctx, runner)
	}
	if dir == m.lastDir {
		m.mu.Unlock()
		return
	}
	m.lastDir = dir
	m.pending = nil
	m.mu.Unlock()

	p := m.propose(ctx, runner, dir)
	if p == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil && m.active.forDir == p.forDir && m.active.hash == p.hash {
		return // already live
	}
	if m.trust.Trusted(p.plugin, p.forDir, p.hash) {
		m.applyLocked(ctx, runner, p)
		return
	}
	m.pending = p
	if key := p.forDir + "\x00" + p.hash; m.notified != key {
		m.notified = key
		fmt.Fprintf(m.notices, "gish: env: plugin %q proposes %d change(s) for %s — run `trust` to review\n",
			p.plugin, len(p.set)+len(p.unset), p.forDir)
	}
}

// propose queries providers in sorted order; the first valid non-empty
// proposal wins. Budget-bounded: a slow provider is skipped for this
// prompt and asked again on the next directory change.
func (m *envManager) propose(ctx context.Context, runner *interp.Runner, dir string) *envProposal {
	select {
	case <-m.warmed:
	case <-time.After(pluginhost.DefaultEnvBudget):
		return nil // warm-up outran the budget; retry next change
	case <-ctx.Done():
		return nil
	}

	req := &pluginapi.EnvDiffRequest{
		Cwd:      dir,
		Env:      allowedShellEnv(runner),
		EventSeq: m.nextSeq(),
	}
	for _, prov := range m.providers {
		rctx, cancel := context.WithTimeout(ctx, pluginhost.DefaultEnvBudget)
		resp, err := prov.Client.EnvDiff(rctx, req)
		cancel()
		if err != nil || resp.GetForDir() == "" {
			continue
		}
		if !underDir(dir, resp.GetForDir()) {
			continue // a diff must derive from cwd or an ancestor
		}
		p := &envProposal{
			plugin: prov.Plugin,
			forDir: resp.GetForDir(),
			set:    map[string]string{},
		}
		for name, value := range resp.GetSet() {
			if envDenied(name) {
				p.stripped = append(p.stripped, name)
				continue
			}
			p.set[name] = value
		}
		for _, name := range resp.GetUnset() {
			if envDenied(name) {
				p.stripped = append(p.stripped, name)
				continue
			}
			p.unset = append(p.unset, name)
		}
		if len(p.set) == 0 && len(p.unset) == 0 {
			continue // nothing survived stripping
		}
		slices.Sort(p.stripped)
		p.hash = envtrust.Hash(p.set, p.unset)
		return p
	}
	return nil
}

// allowPending records trust for the pending proposal and applies it.
// Reports what happened for the trust builtin to print.
func (m *envManager) allowPending(ctx context.Context, runner *interp.Runner) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pending
	if p == nil {
		return "", fmt.Errorf("nothing pending for %s", displayPath(runner.Dir))
	}
	if err := m.trust.Allow(p.plugin, p.forDir, p.hash); err != nil {
		return "", err
	}
	// One gesture, both trust models (#137). A plugin wrapping a tool
	// with its own approval — direnv is the motivating case — gets told
	// the user said yes, so nobody is asked twice for one action. The
	// call is best-effort: gish's own record is authoritative for gish,
	// so a plugin that cannot record it still gets its diff applied,
	// with a note, because the alternative is refusing an approval the
	// user already gave.
	note := m.notifyAllowed(ctx, runner, p)

	m.applyLocked(ctx, runner, p)
	m.pending = nil
	msg := fmt.Sprintf("allowed %q for %s — %d change(s) applied", p.plugin, displayPath(p.forDir), len(p.set)+len(p.unset))
	if note != "" {
		msg += "\n" + note
	}
	return msg, nil
}

// notifyAllowed calls the plugin's Allow. It returns a one-line note
// when the plugin could not record the approval, and "" otherwise —
// including when the plugin does not implement Allow at all, which is
// the ordinary case and not worth mentioning.
func (m *envManager) notifyAllowed(ctx context.Context, runner *interp.Runner, p *envProposal) string {
	ctx, cancel := context.WithTimeout(ctx, pluginhost.DefaultEnvBudget*10)
	defer cancel()

	for _, prov := range m.providers {
		if prov.Plugin != p.plugin {
			continue
		}
		resp, err := prov.Client.Allow(ctx, &pluginapi.AllowRequest{
			ForDir: p.forDir,
			Env:    allowedShellEnv(runner),
		})
		if err != nil {
			// Unimplemented is the expected answer from every plugin
			// that has no second trust model; it is not a problem.
			if status.Code(err) == codes.Unimplemented {
				return ""
			}
			return fmt.Sprintf("note: %s could not record the approval: %v", p.plugin, err)
		}
		if resp.GetRecorded() {
			return ""
		}
		if detail := resp.GetDetail(); detail != "" {
			return fmt.Sprintf("note: %s did not record the approval: %s", p.plugin, detail)
		}
		return fmt.Sprintf("note: %s did not record the approval", p.plugin)
	}
	return ""
}

// revokeDir removes trust for dir and reverts the active diff if it
// derives from that directory.
func (m *envManager) revokeDir(ctx context.Context, runner *interp.Runner, dir string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	removed, err := m.trust.Revoke(dir)
	if err != nil {
		return false, err
	}
	if m.active != nil && m.active.forDir == dir {
		m.revertLocked(ctx, runner)
	}
	m.notified = ""
	return removed, nil
}

// applyLocked snapshots every touched variable and runs the diff in the
// session. Requires m.mu.
func (m *envManager) applyLocked(ctx context.Context, runner *interp.Runner, p *envProposal) {
	if m.active != nil {
		m.revertLocked(ctx, runner) // one live diff at a time
	}
	saved := map[string]*string{}
	snapshot := func(name string) {
		if v, ok := runner.Vars[name]; ok && v.IsSet() {
			s := v.String()
			saved[name] = &s
		} else {
			saved[name] = nil
		}
	}
	for name := range p.set {
		snapshot(name)
	}
	for _, name := range p.unset {
		snapshot(name)
	}
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(p.set)) {
		quoted, err := syntax.Quote(p.set[name], syntax.LangBash)
		if err != nil {
			continue // unquotable value: skip the variable, not the diff
		}
		b.WriteString("export " + name + "=" + quoted + "\n")
	}
	for _, name := range p.unset {
		b.WriteString("unset " + name + "\n")
	}
	if runEnvScript(ctx, runner, b.String()) != nil {
		return
	}
	m.active = &appliedEnv{plugin: p.plugin, forDir: p.forDir, hash: p.hash, saved: saved}
}

// revertLocked restores the snapshot taken at apply time. Requires m.mu.
func (m *envManager) revertLocked(ctx context.Context, runner *interp.Runner) {
	if m.active == nil {
		return
	}
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(m.active.saved)) {
		prev := m.active.saved[name]
		if prev == nil {
			b.WriteString("unset " + name + "\n")
			continue
		}
		quoted, err := syntax.Quote(*prev, syntax.LangBash)
		if err != nil {
			continue
		}
		b.WriteString("export " + name + "=" + quoted + "\n")
	}
	_ = runEnvScript(ctx, runner, b.String()) //nolint:errcheck // best-effort restore
	m.active = nil
}

// runEnvScript parses and runs a synthesized script in the session
// runner (the loadRC mechanism: vars and exports persist).
func runEnvScript(ctx context.Context, runner *interp.Runner, script string) error {
	if script == "" {
		return nil
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(script), "env-diff")
	if err != nil {
		return err
	}
	return runner.Run(ctx, file)
}

// allowedShellEnv is the filtered-env invariant for env plugins: the
// same allowlist plugin commands receive, sourced from the session.
func allowedShellEnv(runner *interp.Runner) map[string]string {
	out := map[string]string{}
	for _, name := range []string{"PATH", "HOME", "TERM", "LANG", "LC_ALL", "USER"} {
		if v, ok := runner.Vars[name]; ok && v.IsSet() {
			out[name] = v.String()
		}
	}
	return out
}

// secretishEnv matches env names that look credential-bearing — the
// deny side of segment env requests, whatever a plugin declares.
var secretishEnv = regexp.MustCompile(`(?i)(secret|token|passw|credential|api[_-]?key|access[_-]?key|private)`)

// requestedEnv resolves a segment's declared env keys against the
// session, refusing deny-listed and secret-shaped names.
func requestedEnv(runner *interp.Runner, keys []string) map[string]string {
	out := map[string]string{}
	for _, key := range keys {
		if envDenied(key) || secretishEnv.MatchString(key) {
			continue
		}
		if v, ok := runner.Vars[key]; ok && v.IsSet() {
			out[key] = v.String()
		}
	}
	return out
}

func underDir(dir, root string) bool {
	if dir == root {
		return true
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
