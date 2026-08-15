package repl

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

// Plugin themes (#30): a tier-2 ThemeProvider renders the entire prompt
// set. Same discipline as prompt segments — every render carries a
// deadline derived from the theme's declared budget, and a miss serves
// the previous prompt set (stale) or reports failure so resolution can
// fall back to the built-in theme. The prompt never waits.

// promptSet is one rendered prompt triple.
type promptSet struct {
	prompt, cont, rprompt string
}

// pluginThemes is the seam promptStrings resolves unknown theme names
// through; nil (tests, no plugin host) means no plugin themes.
type pluginThemes interface {
	render(ctx context.Context, name string, info promptInfo) (promptSet, bool)
}

// themePlugins is set once at interactive startup when a plugin host
// exists. Package-level like the starship renderer: prompt resolution
// is single-threaded per session.
var themePlugins pluginThemes

// themeRenderer maps discovered theme names to their providers.
// Discovery warms in the background from shell startup (#37); the first
// render waits for warm-up only within its budget — a theme missing at
// first paint appears on the next prompt.
type themeRenderer struct {
	host    *pluginhost.Host
	nextSeq func() uint64

	warmed chan struct{} // closed when discovery finishes

	mu     sync.Mutex
	themes map[string]themeEntry
	last   map[string]promptSet // stale fallback per theme name
}

type themeEntry struct {
	client pluginapi.ThemeProviderClient
	budget time.Duration
}

func newThemeRenderer(host *pluginhost.Host) *themeRenderer {
	r := &themeRenderer{
		host:    host,
		nextSeq: host.NextSeq,
		warmed:  make(chan struct{}),
		themes:  map[string]themeEntry{},
		last:    map[string]promptSet{},
	}
	go r.warm()
	return r
}

// warm maps theme names to their providers once, off the startup path.
// Built-in names (plain, p10k, starship) cannot be claimed.
func (r *themeRenderer) warm() {
	defer close(r.warmed)
	ctx, cancel := context.WithTimeout(context.Background(), pluginhost.DescribeTimeout)
	defer cancel()
	for _, prov := range r.host.ThemeProviders(ctx) {
		resp, err := prov.Client.Themes(ctx, &pluginapi.ThemesRequest{})
		if err != nil {
			continue
		}
		r.mu.Lock()
		for _, th := range resp.GetThemes() {
			name := th.GetName()
			if _, isBuiltin := builtinThemes[name]; isBuiltin {
				continue
			}
			budget := pluginhost.DefaultRenderBudget
			if ms := th.GetBudgetMs(); ms > 0 {
				budget = time.Duration(ms) * time.Millisecond
			}
			// First provider claiming a name wins (sorted by plugin name).
			if _, taken := r.themes[name]; !taken {
				r.themes[name] = themeEntry{client: prov.Client, budget: budget}
			}
		}
		r.mu.Unlock()
	}
}

// render returns the theme's current prompt set. Unknown names report
// false (resolution falls back to the built-in theme); budget misses
// serve the previous set. If discovery is still warming up, render
// waits for it only within the default budget.
func (r *themeRenderer) render(ctx context.Context, name string, info promptInfo) (promptSet, bool) {
	select {
	case <-r.warmed:
	case <-time.After(pluginhost.DefaultRenderBudget):
		return r.stale(name) // warm-up outran the budget: paint now, theme next prompt
	case <-ctx.Done():
		return r.stale(name)
	}

	r.mu.Lock()
	entry, ok := r.themes[name]
	r.mu.Unlock()
	if !ok {
		return promptSet{}, false
	}
	pctx := promptContext(info)
	pctx.EventSeq = r.seq()
	rctx, cancel := context.WithTimeout(ctx, entry.budget)
	defer cancel()
	resp, err := entry.client.RenderPrompt(rctx, &pluginapi.RenderPromptRequest{
		Theme:   name,
		Context: pctx,
	})
	if err != nil || resp.GetPrompt() == "" { // a theme must produce a prompt
		return r.stale(name)
	}
	set := promptSet{prompt: resp.GetPrompt(), cont: resp.GetContPrompt(), rprompt: resp.GetRprompt()}
	r.mu.Lock()
	r.last[name] = set
	r.mu.Unlock()
	return set, true
}

func (r *themeRenderer) stale(name string) (promptSet, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set, ok := r.last[name]
	return set, ok
}

func (r *themeRenderer) seq() uint64 {
	if r.nextSeq == nil {
		return 0
	}
	return r.nextSeq()
}

// promptContext converts the shell's prompt state to the wire shape.
// Color is always true here: colorless terminals never reach a theme.
func promptContext(info promptInfo) *pluginapi.PromptContext {
	return &pluginapi.PromptContext{
		Cwd:          info.dir,
		Home:         info.home,
		LastExitCode: int32(info.exitCode),
		DurationMs:   uint64(info.duration.Milliseconds()), //nolint:gosec // durations are non-negative
		Jobs:         uint32(max(info.jobs, 0)),            //nolint:gosec // clamped
		Username:     info.username,
		Hostname:     info.host,
		Ssh:          sshSession(),
		Width:        uint32(max(info.width, 0)), //nolint:gosec // clamped
		Color:        true,
	}
}

func sshSession() bool {
	// Same signal the built-in theme uses for the user@host prefix.
	return os.Getenv("SSH_CONNECTION") != ""
}
