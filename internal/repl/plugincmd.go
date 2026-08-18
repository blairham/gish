package repl

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/manifest"
	"github.com/blairham/koi-shell/internal/plugmgr"
)

// The plugin surface (#108): the declarative manifest replaces zi's
// ice-modifier language as the documented way to manage plugins. Four
// knobs — source, kind, pin, lazy — and commands that edit the file for
// you, the same shape as `config` and `tool`.
//
// The zi engine still installs and sources everything; what changed is
// that a user configures plugins by naming what they want rather than
// by learning a modifier vocabulary.

// pluginMgr is set at interactive startup: the manifest plus the lazy
// triggers still waiting for their command.
var pluginMgr *pluginManager

type pluginManager struct {
	path string
	man  *manifest.Manifest
	mgr  plugmgr.Manager

	mu sync.Mutex
	// pending maps a trigger command to the entry that loads on it.
	pending map[string]manifest.Plugin
}

// newPluginManager loads the manifest and returns the manager, or nil
// when the manifest cannot be read (the error is the caller's to
// report — a broken plugin list must not be silent).
func newPluginManager(mgr plugmgr.Manager) (*pluginManager, error) {
	path, err := manifest.DefaultPath()
	if err != nil {
		return nil, err
	}
	man, err := manifest.Load(path)
	if err != nil {
		return nil, err
	}
	return &pluginManager{path: path, man: man, mgr: mgr, pending: map[string]manifest.Plugin{}}, nil
}

// loadEager installs and sources every enabled non-lazy entry, and
// registers the lazy ones for their trigger. It returns the shell
// source lines to run, because sourcing must happen in the session.
func (p *pluginManager) loadEager() []string {
	var lines []string
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.man.Plugins {
		if !entry.On() {
			continue
		}
		if trigger, ok := strings.CutPrefix(entry.Lazy, "command:"); ok && trigger != "" {
			p.pending[trigger] = entry
			continue
		}
		if payload, err := p.install(entry); err == nil && payload != "" {
			lines = append(lines, payload)
		}
	}
	return lines
}

// install resolves one entry through the zi engine and returns the
// payload path to source ("" for a release binary, which only extends
// PATH).
func (p *pluginManager) install(entry manifest.Plugin) (string, error) {
	var ices []string
	switch entry.EffectiveKind() {
	case manifest.KindRelease:
		ices = append(ices, `from"gh-r"`, `as"program"`)
	case manifest.KindSnippet:
		if entry.Pin != "" {
			ices = append(ices, `ver"`+entry.Pin+`"`)
		}
		if err := p.mgr.SetIces(ices); err != nil {
			return "", err
		}
		return p.mgr.Snippet(entry.Source)
	case manifest.KindPlugin:
	}
	if entry.Pin != "" {
		ices = append(ices, `ver"`+entry.Pin+`"`)
	}
	if err := p.mgr.SetIces(ices); err != nil {
		return "", err
	}
	return p.mgr.Load(entry.Source)
}

// lazyCallHandler loads a deferred plugin the first time its trigger
// command runs, then runs the command — the payload is sourced in the
// live session, so functions and PATH edits are in place before the
// command resolves.
func lazyCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if pluginMgr == nil || len(args) == 0 {
			return next(ctx, args)
		}
		entry, ok := pluginMgr.take(args[0])
		if !ok {
			return next(ctx, args)
		}
		payload, err := pluginMgr.install(entry)
		if err != nil {
			fmt.Fprintf(interp.HandlerCtx(ctx).Stderr, "koi: plugin %s: %v\n", entry.Name(), err)
			return next(ctx, args)
		}
		if payload == "" {
			return next(ctx, args) // release binary: PATH is enough
		}
		quoted := make([]string, 0, len(args)+1)
		for _, a := range append([]string{payload}, args...) {
			q, qerr := syntax.Quote(a, syntax.LangBash)
			if qerr != nil {
				return next(ctx, args)
			}
			quoted = append(quoted, q)
		}
		// source the payload, then run the original command line.
		return []string{"eval", "source " + quoted[0] + "; " + strings.Join(quoted[1:], " ")}, nil
	}
}

// triggers returns the lazy trigger commands still pending. They
// resolve the moment they run, so the name-judging surfaces (#193)
// treat them as real rather than as typos-until-first-use.
func (p *pluginManager) triggers() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.pending))
	for name := range p.pending {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// take claims a pending trigger, so a plugin loads exactly once.
func (p *pluginManager) take(command string) (manifest.Plugin, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.pending[command]
	if ok {
		delete(p.pending, command)
	}
	return entry, ok
}

const pluginUsage = `usage: plugin [add|remove|pin|enable|disable|update] …

  plugin                          list configured plugins
  plugin browse                   pick what loads, or add from a starter list
  plugin add zsh-users/zsh-autosuggestions
  plugin add junegunn/fzf --kind release --pin 0.55.0
  plugin add ohmyzsh/ohmyzsh --lazy command:git
  plugin remove fzf
  plugin pin fzf 0.55.0           change the version
  plugin enable|disable fzf       keep the entry, toggle loading
  plugin update [name]            refresh installs

Four knobs, no modifier language: source, kind (plugin|release|snippet),
pin, lazy (command:NAME). The file is $XDG_CONFIG_HOME/koi/plugins.toml
and hand edits are equally fine.`

// pluginCallHandler intercepts `plugin`, config-style.
func pluginCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "plugin" {
			return next(ctx, args)
		}
		return runPlugin(ctx, interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runPlugin(ctx context.Context, hc interp.HandlerContext, args []string) []string {
	fail := func(err error) []string {
		fmt.Fprintln(hc.Stderr, "plugin:", err)
		return []string{"false"}
	}
	if pluginMgr == nil {
		// Non-interactive sessions manage plugins too (scripts, `koi -c`
		// in a dotfiles bootstrap); only *loading* is interactive.
		mgr, err := plugmgr.NewZi(hc.Stderr)
		if err != nil {
			return fail(err)
		}
		if pluginMgr, err = newPluginManager(mgr); err != nil {
			return fail(err)
		}
	}

	switch {
	case len(args) == 0:
		pluginMgr.list(hc)
		return []string{"true"}

	case args[0] == "help", args[0] == "-h", args[0] == "--help":
		fmt.Fprintln(hc.Stdout, pluginUsage)
		return []string{"true"}

	case args[0] == "browse":
		// The interactive manager (#90). It edits the same manifest the
		// other subcommands do — the form is a way of filling it in, not
		// a second source of truth.
		return runPluginBrowse(handlerIO{Stdin: hc.Stdin, Stdout: hc.Stdout, Stderr: hc.Stderr},
			pluginMgr.path, pluginMgr.man)

	case args[0] == "add" && len(args) >= 2:
		entry, err := parseAdd(args[1:])
		if err != nil {
			return fail(err)
		}
		replaced := pluginMgr.man.Add(entry)
		if err := pluginMgr.man.Save(pluginMgr.path); err != nil {
			return fail(err)
		}
		verb := "added"
		if replaced {
			verb = "updated"
		}
		fmt.Fprintf(hc.Stdout, "%s %s — saved to %s\n", verb, entry.Source, displayPath(pluginMgr.path))
		// Install now so the entry is usable without a restart; a lazy
		// entry just registers its trigger.
		if entry.On() && entry.Lazy == "" {
			payload, ierr := pluginMgr.install(entry)
			if ierr != nil {
				return fail(ierr)
			}
			if payload != "" {
				return []string{"source", payload}
			}
		} else if entry.On() {
			if trigger, ok := strings.CutPrefix(entry.Lazy, "command:"); ok {
				pluginMgr.mu.Lock()
				pluginMgr.pending[trigger] = entry
				pluginMgr.mu.Unlock()
			}
		}
		return []string{"true"}

	case args[0] == "remove" && len(args) == 2:
		if !pluginMgr.man.Remove(args[1]) {
			return fail(fmt.Errorf("%q is not in the manifest", args[1]))
		}
		if err := pluginMgr.man.Save(pluginMgr.path); err != nil {
			return fail(err)
		}
		fmt.Fprintf(hc.Stdout, "removed %s (installed files stay; `zi delete` clears them)\n", args[1])
		return []string{"true"}

	case args[0] == "pin" && len(args) == 3:
		return pluginMgr.mutate(hc, fail, args[1], func(p *manifest.Plugin) { p.Pin = args[2] },
			"pinned %s to "+args[2])

	case args[0] == "enable" && len(args) == 2:
		return pluginMgr.mutate(hc, fail, args[1], func(p *manifest.Plugin) { p.Enabled = nil }, "enabled %s")

	case args[0] == "disable" && len(args) == 2:
		off := false
		return pluginMgr.mutate(hc, fail, args[1], func(p *manifest.Plugin) { p.Enabled = &off }, "disabled %s")

	case args[0] == "update":
		target := ""
		if len(args) == 2 {
			target = args[1]
		}
		if err := ziUpdate(pluginMgr.mgr, target, hc); err != nil {
			return fail(err)
		}
		return []string{"true"}

	default:
		return fail(fmt.Errorf("unknown arguments %q\n%s", strings.Join(args, " "), pluginUsage))
	}
}

// mutate applies a change to one entry and persists.
func (p *pluginManager) mutate(
	hc interp.HandlerContext, fail func(error) []string,
	name string, apply func(*manifest.Plugin), msg string,
) []string {
	i := p.man.Find(name)
	if i < 0 {
		return fail(fmt.Errorf("%q is not in the manifest", name))
	}
	apply(&p.man.Plugins[i])
	if err := p.man.Save(p.path); err != nil {
		return fail(err)
	}
	fmt.Fprintf(hc.Stdout, msg+"\n", p.man.Plugins[i].Source)
	return []string{"true"}
}

// list prints the manifest with each entry's state.
func (p *pluginManager) list(hc interp.HandlerContext) {
	if len(p.man.Plugins) == 0 {
		fmt.Fprintf(hc.Stdout, "no plugins configured — `plugin add user/repo` writes %s\n",
			displayPath(p.path))
		return
	}
	for _, entry := range p.man.Plugins {
		state := string(entry.EffectiveKind())
		if !entry.On() {
			state += ", disabled"
		}
		if entry.Lazy != "" {
			state += ", lazy on " + strings.TrimPrefix(entry.Lazy, "command:")
		}
		version := entry.Pin
		if version == "" {
			version = "latest"
		}
		fmt.Fprintf(hc.Stdout, "  %-40s %-10s (%s)\n", entry.Source, version, state)
	}
}

// parseAdd builds an entry from `add SOURCE [--kind K] [--pin V] [--lazy T]`.
func parseAdd(args []string) (manifest.Plugin, error) {
	entry := manifest.Plugin{Source: args[0]}
	rest := args[1:]
	for len(rest) > 0 {
		if len(rest) < 2 {
			return entry, fmt.Errorf("%s needs a value", rest[0])
		}
		switch rest[0] {
		case "--kind":
			entry.Kind = manifest.Kind(rest[1])
			if k := entry.Kind; k != manifest.KindPlugin && k != manifest.KindRelease && k != manifest.KindSnippet {
				return entry, fmt.Errorf("unknown kind %q (plugin, release, snippet)", k)
			}
		case "--pin":
			entry.Pin = rest[1]
		case "--lazy":
			if !strings.HasPrefix(rest[1], "command:") {
				return entry, fmt.Errorf("lazy triggers are command:NAME, got %q", rest[1])
			}
			entry.Lazy = rest[1]
		default:
			return entry, fmt.Errorf("unknown flag %q\n%s", rest[0], pluginUsage)
		}
		rest = rest[2:]
	}
	return entry, nil
}
