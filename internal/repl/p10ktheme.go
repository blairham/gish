package repl

import (
	"os"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/p10k"
)

// The p10k theme's front door in the shell.
//
// GISH_THEME=p10k renders through internal/p10k in this process: no
// subprocess, no plugin, no IPC on the prompt path. The engine is shared
// with cmd/gish-p10k, which serves the same themes to other shells over
// the tier-2 contract; this path exists because the shell's own prompt
// should not pay a round trip to render itself.
//
// Configuration layers, later winning over earlier:
//
//	1. the preset named by GISH_P10K_PRESET (lean by default)
//	2. the native config file, when one exists
//	3. POWERLEVEL9K_* set in the session
//
// Layer 3 is there because it is what a person arriving from
// powerlevel10k already knows how to type. It reads settings as *data* —
// there is no shell being interpreted, and a .p10k.zsh is not consulted
// here; importing one is an explicit, one-time step (`p10k import`).

// presetCache holds built presets so a prompt costs a map lookup rather
// than a rebuild. Presets are pure data and never mutated in place —
// anything layered on top happens on a clone.
var presetCache sync.Map // preset name -> *p10k.Config

// p10kTheme is the builtinThemes entry for "p10k".
func p10kTheme(runner *interp.Runner, info promptInfo) (string, string, string) {
	out := p10k.Render(p10kConfigFor(runner), p10kContext(runner, info))
	return out.Prompt, out.Cont, out.RPrompt
}

// p10kConfigFor assembles the configuration for this prompt.
func p10kConfigFor(runner *interp.Runner) *p10k.Config {
	// A nil runner means a caller outside the prompt loop (`p10k show`),
	// which reads the same settings from the environment.
	name := os.Getenv("GISH_P10K_PRESET")
	if runner != nil {
		name = shellVar(runner, "GISH_P10K_PRESET", p10k.DefaultPreset)
	}
	if name == "" {
		name = p10k.DefaultPreset
	}
	cached, ok := presetCache.Load(name)
	if !ok {
		cfg := p10k.Preset(name)
		if cfg == nil {
			// An unknown preset name degrades to the default rather than
			// to no prompt; doctor reports the typo.
			cfg = p10k.Preset(p10k.DefaultPreset)
		}
		presetCache.Store(name, cfg)
		cached = cfg
	}
	base := cached.(*p10k.Config)

	// The file layer is mtime-cached rather than session-cached, so an
	// edit shows up on the next prompt instead of the next shell.
	file := p10k.LoadNativeConfig()
	overrides := sessionOverrides(runner)
	if file == nil && overrides == nil {
		return base
	}
	merged := base.Clone()
	merged.Merge(file)
	merged.Merge(overrides)
	return merged
}

// sessionOverrides collects POWERLEVEL9K_* settings from the session,
// returning nil when there are none — the common case, which then costs
// no clone and no merge.
func sessionOverrides(runner *interp.Runner) *p10k.Config {
	var out *p10k.Config
	each := func(name, value string) {
		key, ok := strings.CutPrefix(name, "POWERLEVEL9K_")
		if !ok {
			return
		}
		if out == nil {
			out = p10k.NewConfig()
			out.Sources = append(out.Sources, "session")
		}
		// Element lists are the one place a value is plural. Everything
		// else is a scalar, so splitting is opt-in by key rather than by
		// guessing at the contents.
		if strings.HasSuffix(key, "_PROMPT_ELEMENTS") {
			out.SetList(key, strings.Fields(value))
			return
		}
		out.Set(key, value)
	}

	if runner == nil {
		for _, kv := range os.Environ() {
			if name, value, found := strings.Cut(kv, "="); found {
				each(name, value)
			}
		}
		return out
	}
	// Shell variables first, then the inherited environment — the same
	// precedence shellVar uses, so one rule governs both.
	if runner.Env != nil {
		runner.Env.Each(func(name string, v expand.Variable) bool {
			each(name, v.String())
			return true
		})
	}
	for name, v := range runner.Vars {
		each(name, v.String())
	}
	return out
}

// p10kContext converts the shell's prompt state into what a segment is
// allowed to see.
func p10kContext(runner *interp.Runner, info promptInfo) *p10k.Context {
	dir := info.dir
	if dir == "" && runner != nil {
		dir = runner.Dir
	}
	ctx := &p10k.Context{
		Cwd:      dir,
		Home:     info.home,
		Username: info.username,
		Hostname: info.host,
		ExitCode: info.exitCode,
		Duration: info.duration,
		Jobs:     info.jobs,
		Width:    info.width,
		Root:     os.Geteuid() == 0,
		SSH:      sshSession(),
	}
	if runner != nil {
		ctx.Getenv = func(name string) string { return shellVar(runner, name, "") }
	}
	// The branch is read here and now — one cached file read. Counts come
	// from whoever is scanning the repository and are merged when they
	// describe this same working tree.
	ctx.Git = p10k.HeadStatus(dir)
	return ctx
}
