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

// lastPromptDir is the directory the previous prompt rendered in. The
// same-dir transient mode needs it, and prompt resolution is
// single-threaded per session, so a package variable is the honest
// shape — the same one starship and the segment renderer already use.
//
// A note on what same-dir can mean here. Upstream describes it as "trim
// the prompt unless the command changed directory", but the trim happens
// when the line is accepted, before the command has run — nobody can
// know yet. So this reads it the other way round: a prompt is trimmed
// unless the *previous* command changed directory. Either way a
// directory change leaves one full prompt in the scrollback as a
// landmark, which is the point of the setting; the landmark lands just
// below the cd rather than just above it.
var lastPromptDir string

// gitCounts is the session's background git-status scanner (#130). One
// per process: its whole value is the cache, and the cache is keyed by
// repository, so every prompt in every directory shares it.
var gitCounts = &p10k.CountScanner{}

// p10kTheme is the builtinThemes entry for "p10k".
func p10kTheme(runner *interp.Runner, info promptInfo) (string, string, string) {
	out := p10k.Render(p10kConfigFor(runner), p10kContext(runner, info))
	return out.Prompt, out.Cont, out.RPrompt
}

// p10kConfigFor assembles the configuration for this prompt.
func p10kConfigFor(runner *interp.Runner) *p10k.Config {
	return p10kConfig(shellVar(runner, "GISH_P10K_PRESET", p10k.DefaultPreset), sessionOverrides(runner))
}

// p10kConfigFromEnv is the same assembly for callers that hold a handler
// context rather than the runner (`p10k show`). It has to exist: a
// setting assigned in the session is a *shell* variable, not an exported
// one, so reading os.Environ would report a configuration the prompt is
// not actually using.
func p10kConfigFromEnv(env expand.Environ) *p10k.Config {
	name := env.Get("GISH_P10K_PRESET").String()
	if name == "" {
		name = p10k.DefaultPreset
	}
	var overrides *p10k.Config
	env.Each(func(varName string, v expand.Variable) bool {
		overrides = addOverride(overrides, varName, v.String())
		return true
	})
	return p10kConfig(name, overrides)
}

// p10kConfig layers the preset, the config file and any session
// overrides into the configuration one render will read.
func p10kConfig(name string, overrides *p10k.Config) *p10k.Config {
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
	if runner == nil {
		for _, kv := range os.Environ() {
			if name, value, found := strings.Cut(kv, "="); found {
				out = addOverride(out, name, value)
			}
		}
		return out
	}
	// The inherited environment first, then shell variables on top — the
	// same precedence shellVar uses, so one rule governs both.
	if runner.Env != nil {
		runner.Env.Each(func(name string, v expand.Variable) bool {
			out = addOverride(out, name, v.String())
			return true
		})
	}
	for name, v := range runner.Vars {
		out = addOverride(out, name, v.String())
	}
	return out
}

// addOverride records one POWERLEVEL9K_* setting, allocating the config
// only once something actually matches — the common case is that nothing
// does, and a prompt should not pay for a map it will not use.
func addOverride(out *p10k.Config, name, value string) *p10k.Config {
	key, ok := strings.CutPrefix(name, "POWERLEVEL9K_")
	if !ok {
		return out
	}
	if out == nil {
		out = p10k.NewConfig()
		out.Sources = append(out.Sources, "session")
	}
	// Element lists are the one place a value is plural. Everything else
	// is a scalar, so splitting is opt-in by key rather than a guess at
	// the contents.
	if strings.HasSuffix(key, "_PROMPT_ELEMENTS") {
		out.SetList(key, strings.Fields(value))
		return out
	}
	out.Set(key, value)
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
	ctx.PrevCwd = lastPromptDir
	// The branch is read here and now — one cached file read. Counts come
	// from whoever is scanning the repository and are merged when they
	// describe this same working tree.
	ctx.Git = p10k.HeadStatus(dir)
	// The counts are the expensive half (#130): scanned in the
	// background, merged when they describe this same working tree, and
	// marked stale rather than presented as current when the tree has
	// moved since. The prompt reads what is known now and never waits.
	if ctx.Git != nil {
		if counts, ok := gitCounts.Counts(dir); ok {
			ctx.Git.MergeCounts(counts)
		}
	}
	return ctx
}
