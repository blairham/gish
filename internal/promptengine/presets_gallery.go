package promptengine

// Presets from upstreams other than powerlevel10k.
//
// These are the point of modeling configuration generically: the layout
// pass in render.go already spans "no backgrounds, space separator"
// (lean) to "a background per segment, powerline arrows" (rainbow), and
// every preset in starship's gallery is one of those two shapes. So a
// look from another project costs data and no code — which is the claim
// presets.go has been making since it was written, on the evidence of
// six presets from a single upstream. These are the second and third.
//
// They live in their own file because their provenance differs, and
// provenance is the thing you have to be able to answer later:
//
//   - pastel-powerline and tokyo-night are transcribed from starship's
//     own preset TOMLs (ISC, starship contributors,
//     https://github.com/starship/starship). Every color here is
//     upstream's, copied digit for digit.
//   - agnoster is *not* a transcription. The oh-my-zsh theme is a zsh
//     program, not a config file (it defines functions and shells out to
//     git), so there is nothing to transcribe — this rebuilds the look
//     from its published appearance. It is labeled that way below and
//     in `prompt show`, because "we reproduced the look" and "we took
//     their config" are different claims and only one of them is true.
//
// The honesty rule from `prompt import` applies to every preset here: a
// look is either faithful or it says what it dropped. Where an upstream
// preset shows something this engine will not render, the modules are
// named in Unsupported, and `prompt preset` prints them as it applies
// the look — rather than the prompt quietly coming up short a segment
// and the user wondering whether they configured it wrong.
//
// Note that this reporting happens at apply time and *not* in
// `prompt show`. SaveNativeConfig writes a resolved list of settings, so
// Unsupported does not survive the round trip: read the file back and
// there is no way to know what the preset chose not to carry. Apply time
// is the actionable moment anyway — that is when the user still has the
// screenshot they picked the look from in front of them.
//
// The recurring gap is the same one AGENTS.md already records: the
// `*_version` family. starship runs `node --version`, `go version`,
// `rustc --version` and friends on the prompt path; no segment in this
// package forks, ever, which is where the speed comes from. gish's
// nearest equivalents (nodenv, pyenv, goenv, asdf…) read a pin *file*,
// which answers a different question — "what is this project pinned to"
// rather than "what is on PATH right now" — so they are not substituted
// in silently.

// galleryUnsupported records upstream modules a preset shows and this
// engine does not, in the vocabulary of the upstream they came from —
// someone comparing against a screenshot needs the name they would
// search for, not gish's name for the nearest thing.
func galleryUnsupported(cfg *Config, upstream string, modules ...string) {
	for _, m := range modules {
		cfg.Unsupported = append(cfg.Unsupported, upstream+" "+m+" (needs a subprocess; no segment here forks)")
	}
}

// presetPastelPowerline is starship's pastel-powerline, transcribed from
// its preset TOML. The ribbon runs purple → pink → orange → blue → teal
// → navy, and those six colors are the whole identity of the look.
//
// Upstream shows no prompt character: its format ends with the closing
// separator and you type immediately after the ribbon. That is
// reproduced rather than corrected — a preset that quietly adds a `❯`
// is not the preset someone asked for.
func presetPastelPowerline() *Config {
	c := baseConfig()
	powerlineShape(c)
	c.SetList("LEFT_PROMPT_ELEMENTS", []string{"os_icon", "context", "dir", "vcs", "time"})
	c.SetList("RIGHT_PROMPT_ELEMENTS", nil)
	c.Set("PROMPT_ADD_NEWLINE", "false")

	c.Set("OS_ICON_BACKGROUND", "#9a348e")
	c.Set("CONTEXT_BACKGROUND", "#9a348e")
	c.Set("CONTEXT_ALWAYS_SHOW", "true")
	c.Set("CONTEXT_TEMPLATE", "%n")

	c.Set("DIR_BACKGROUND", "#da627d")
	c.Set("SHORTEN_DIR_LENGTH", "3")
	c.Set("SHORTEN_STRATEGY", "truncate_to_last")

	for _, state := range []string{"CLEAN", "MODIFIED", "UNTRACKED", "CONFLICTED"} {
		c.Set("VCS_"+state+"_BACKGROUND", "#fca17d")
	}

	c.Set("TIME_BACKGROUND", "#33658a")
	c.Set("TIME_FORMAT", "%H:%M")

	galleryUnsupported(c, "starship",
		"$c", "$elixir", "$elm", "$golang", "$gradle", "$haskell", "$java",
		"$julia", "$maven", "$nodejs", "$bun", "$nim", "$rust", "$scala",
		"$docker_context")
	return c
}

// presetTokyoNight is starship's tokyo-night, transcribed from its
// preset TOML: a cool, dark ribbon that fades from the slate os block
// into near-black by the time it reaches the clock.
//
// Unlike pastel-powerline this one is two lines — upstream's format ends
// with `\n$character` — so the prompt character comes back.
func presetTokyoNight() *Config {
	c := baseConfig()
	powerlineShape(c)
	c.SetList("LEFT_PROMPT_ELEMENTS", []string{"os_icon", "dir", "vcs", "time", elementNewline, "prompt_char"})
	c.SetList("RIGHT_PROMPT_ELEMENTS", nil)
	c.Set("PROMPT_ADD_NEWLINE", "false")

	c.Set("OS_ICON_BACKGROUND", "#a3aed2")
	c.Set("OS_ICON_FOREGROUND", "#090c0c")

	c.Set("DIR_BACKGROUND", "#769ff0")
	c.Set("DIR_FOREGROUND", "#e3e5e5")
	c.Set("DIR_SHORTENED_FOREGROUND", "#e3e5e5")
	c.Set("DIR_ANCHOR_FOREGROUND", "#e3e5e5")
	c.Set("SHORTEN_DIR_LENGTH", "3")
	c.Set("SHORTEN_STRATEGY", "truncate_to_last")

	for _, state := range []string{"CLEAN", "MODIFIED", "UNTRACKED", "CONFLICTED"} {
		c.Set("VCS_"+state+"_BACKGROUND", "#394260")
		c.Set("VCS_"+state+"_FOREGROUND", "#769ff0")
	}

	c.Set("TIME_BACKGROUND", "#1d2230")
	c.Set("TIME_FOREGROUND", "#a0a9cb")
	c.Set("TIME_FORMAT", "%H:%M")

	c.Set("PROMPT_CHAR_OK_FOREGROUND", "#9ece6a")
	c.Set("PROMPT_CHAR_ERROR_FOREGROUND", "#f7768e")

	galleryUnsupported(c, "starship", "$nodejs", "$bun", "$rust", "$golang", "$php")
	return c
}

// presetAgnoster rebuilds oh-my-zsh's agnoster look: the powerline
// prompt most people picture when they picture a themed zsh.
//
// This is a reconstruction, not a transcription, and the difference is
// load-bearing. agnoster.zsh-theme is a zsh program — it defines
// prompt_segment(), runs `git rev-parse`, and hooks precmd — so there is
// no config to import and no general converter that could produce this
// (see #185). What is reproduced is the published appearance: the
// user@host block, a blue directory, and a git block that goes yellow
// when the tree is dirty, all joined by solid arrows.
//
// Two of agnoster's blocks are deliberately absent rather than faked:
// its virtualenv block and its root/background-jobs status glyphs sit in
// a different place in the ribbon than this engine's equivalents, and
// putting gish's versions in agnoster's colors would produce something
// that is neither.
func presetAgnoster() *Config {
	c := baseConfig()
	powerlineShape(c)
	c.SetList("LEFT_PROMPT_ELEMENTS", []string{"context", "dir", "vcs"})
	c.SetList("RIGHT_PROMPT_ELEMENTS", nil)
	c.Set("PROMPT_ADD_NEWLINE", "false")

	// user@host: white on black, the block agnoster always opens with.
	c.Set("CONTEXT_BACKGROUND", "0")
	c.Set("CONTEXT_FOREGROUND", "7")
	c.Set("CONTEXT_ALWAYS_SHOW", "true")
	c.Set("CONTEXT_TEMPLATE", "%n@%m")

	c.Set("DIR_BACKGROUND", "4")
	c.Set("DIR_FOREGROUND", "0")
	c.Set("DIR_ANCHOR_FOREGROUND", "0")
	c.Set("DIR_ANCHOR_BOLD", "true")
	c.Set("DIR_SHORTENED_FOREGROUND", "0")
	c.Set("SHORTEN_STRATEGY", "truncate_to_last")

	// Clean is green, anything outstanding is yellow — agnoster's one
	// piece of real signaling, and the reason people recognize it.
	c.Set("VCS_CLEAN_BACKGROUND", "2")
	c.Set("VCS_CLEAN_FOREGROUND", "0")
	c.Set("VCS_MODIFIED_BACKGROUND", "3")
	c.Set("VCS_MODIFIED_FOREGROUND", "0")
	c.Set("VCS_UNTRACKED_BACKGROUND", "3")
	c.Set("VCS_UNTRACKED_FOREGROUND", "0")
	c.Set("VCS_CONFLICTED_BACKGROUND", "1")
	c.Set("VCS_CONFLICTED_FOREGROUND", "0")
	c.Set("VCS_BRANCH_ICON", " ")

	c.Unsupported = append(c.Unsupported,
		"agnoster is rebuilt from its appearance, not imported — the theme is zsh code, not config (#185)",
		"agnoster virtualenv and root/jobs blocks (they sit elsewhere in the ribbon here)")
	return c
}
