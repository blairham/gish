package p10k

import "sort"

// The built-in presets, as data.
//
// These are the same looks upstream ships, expressed in the parameter
// namespace the renderer already reads. That is the whole payoff of
// modeling configuration generically: a preset adds no code, and a
// user's own settings layer over one of these without either side
// knowing about the other.
//
// Values are transcribed from the upstream configs (MIT). Where a
// setting drives behavior this port does not implement, it is still
// carried, so that `p10k show` reports the truth and a later version can
// honor it without a config migration.

// presetFunc builds a preset's configuration.
type presetFunc func() *Config

var presets = map[string]presetFunc{
	"lean":         presetLean,
	"lean-8colors": presetLean8,
	"classic":      presetClassic,
	"rainbow":      presetRainbow,
	"pure":         presetPure,
	"robbyrussell": presetRobbyRussell,
}

// Presets lists the available preset names, sorted.
func Presets() []string {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Preset returns a preset's configuration, or nil for an unknown name.
func Preset(name string) *Config {
	fn, ok := presets[name]
	if !ok {
		return nil
	}
	cfg := fn()
	cfg.Sources = append(cfg.Sources, "preset:"+name)
	return cfg
}

// DefaultPreset is what a user who has never configured anything gets.
// Lean is upstream's own default for a reason: it needs no special font
// to look right, which makes it the only safe first impression.
const DefaultPreset = "lean"

// baseConfig holds what every preset agrees on, so each preset below
// states only what makes it that preset.
func baseConfig() *Config {
	c := NewConfig()
	c.SetList("LEFT_PROMPT_ELEMENTS", []string{"dir", "vcs", elementNewline, "prompt_char"})
	c.SetList("RIGHT_PROMPT_ELEMENTS", []string{
		"status", "command_execution_time", "background_jobs", "direnv", "asdf",
		"virtualenv", "anaconda", "pyenv", "goenv", "nodenv", "nvm", "rbenv", "rvm",
		"kubecontext", "terraform", "aws", "azure", "gcloud", "google_app_cred",
		"context", "nix_shell", "vim_shell", "todo", "time",
	})
	c.Set("MODE", "nerdfont-v3")
	c.Set("ICON_PADDING", "none")
	c.Set("PROMPT_ADD_NEWLINE", "true")
	c.Set("MULTILINE_FIRST_PROMPT_GAP_CHAR", " ")

	c.Set("DIR_MAX_LENGTH", "80")
	c.Set("SHORTEN_DIR_LENGTH", "1")
	c.Set("VCS_MAX_INDEX_SIZE_DIRTY", "-1")
	c.Set("STATUS_OK", "false")
	c.Set("STATUS_ERROR", "true")
	c.Set("COMMAND_EXECUTION_TIME_THRESHOLD", "3")
	c.Set("COMMAND_EXECUTION_TIME_PRECISION", "0")
	c.Set("BACKGROUND_JOBS_VERBOSE", "false")
	c.Set("CONTEXT_TEMPLATE", "%n@%m")
	c.Set("TIME_FORMAT", "%D{%H:%M:%S}")
	c.Set("TRANSIENT_PROMPT", "off")
	// No INSTANT_PROMPT default: the setting is accepted from imported
	// configurations but not implemented, so a preset must not imply it
	// is doing something. See instantPromptNote in internal/repl.
	return c
}

// leanShape is the geometry shared by lean and its 8-color variant:
// no backgrounds, no separators, segments held apart by a single space.
// Every "style" in this file is really just a choice about these six
// settings plus a palette.
func leanShape(c *Config) {
	c.Set("BACKGROUND", "")
	for _, side := range []string{"LEFT", "RIGHT"} {
		c.Set(side+"_LEFT_WHITESPACE", "")
		c.Set(side+"_RIGHT_WHITESPACE", "")
		c.Set(side+"_SUBSEGMENT_SEPARATOR", " ")
		c.Set(side+"_SEGMENT_SEPARATOR", "")
	}
	c.Set("LEFT_PROMPT_FIRST_SEGMENT_START_SYMBOL", "")
	c.Set("LEFT_PROMPT_LAST_SEGMENT_END_SYMBOL", "")
	c.Set("RIGHT_PROMPT_FIRST_SEGMENT_START_SYMBOL", "")
	c.Set("RIGHT_PROMPT_LAST_SEGMENT_END_SYMBOL", "")
	c.Set("MULTILINE_FIRST_PROMPT_PREFIX", "")
	c.Set("MULTILINE_NEWLINE_PROMPT_PREFIX", "")
	c.Set("MULTILINE_LAST_PROMPT_PREFIX", "")
	c.Set("MULTILINE_FIRST_PROMPT_SUFFIX", "")
	c.Set("MULTILINE_NEWLINE_PROMPT_SUFFIX", "")
	c.Set("MULTILINE_LAST_PROMPT_SUFFIX", "")
}

// powerlineShape is the geometry classic and rainbow share: every
// segment carries a background, and the boundaries between them are
// drawn with the powerline glyphs.
func powerlineShape(c *Config) {
	c.Set("LEFT_SUBSEGMENT_SEPARATOR", ``)
	c.Set("RIGHT_SUBSEGMENT_SEPARATOR", ``)
	c.Set("LEFT_SEGMENT_SEPARATOR", ``)
	c.Set("RIGHT_SEGMENT_SEPARATOR", ``)
	c.Set("LEFT_PROMPT_LAST_SEGMENT_END_SYMBOL", ``)
	c.Set("RIGHT_PROMPT_FIRST_SEGMENT_START_SYMBOL", ``)
	c.Set("LEFT_PROMPT_FIRST_SEGMENT_START_SYMBOL", "")
	c.Set("RIGHT_PROMPT_LAST_SEGMENT_END_SYMBOL", "")
	c.Set("LEFT_LEFT_WHITESPACE", " ")
	c.Set("LEFT_RIGHT_WHITESPACE", " ")
	c.Set("RIGHT_LEFT_WHITESPACE", " ")
	c.Set("RIGHT_RIGHT_WHITESPACE", " ")
	// The prompt character sits outside the ribbon.
	c.Set("PROMPT_CHAR_BACKGROUND", "")
	c.Set("PROMPT_CHAR_LEFT_PROMPT_LAST_SEGMENT_END_SYMBOL", "")
	c.Set("PROMPT_CHAR_LEFT_PROMPT_FIRST_SEGMENT_START_SYMBOL", "")
	c.Set("PROMPT_CHAR_LEFT_LEFT_WHITESPACE", "")
	c.Set("PROMPT_CHAR_LEFT_RIGHT_WHITESPACE", "")
}

// frame draws the ╭─ ╰─ box that classic and rainbow put around the
// prompt, in color c.
func frame(cfg *Config, color string) {
	cfg.Set("MULTILINE_FIRST_PROMPT_PREFIX", "%"+color+"F╭─")
	cfg.Set("MULTILINE_NEWLINE_PROMPT_PREFIX", "%"+color+"F├─")
	cfg.Set("MULTILINE_LAST_PROMPT_PREFIX", "%"+color+"F╰─")
	cfg.Set("MULTILINE_FIRST_PROMPT_SUFFIX", "%"+color+"F─╮")
	cfg.Set("MULTILINE_NEWLINE_PROMPT_SUFFIX", "%"+color+"F─┤")
	cfg.Set("MULTILINE_LAST_PROMPT_SUFFIX", "%"+color+"F─╯")
}

func presetLean() *Config {
	c := baseConfig()
	leanShape(c)
	c.Set("DIR_FOREGROUND", "31")
	c.Set("DIR_SHORTENED_FOREGROUND", "103")
	c.Set("DIR_ANCHOR_FOREGROUND", "39")
	c.Set("DIR_ANCHOR_BOLD", "true")
	c.Set("VCS_CLEAN_FOREGROUND", "76")
	c.Set("VCS_UNTRACKED_FOREGROUND", "76")
	c.Set("VCS_MODIFIED_FOREGROUND", "178")
	c.Set("VCS_CONFLICTED_FOREGROUND", "196")
	c.Set("STATUS_OK_FOREGROUND", "70")
	c.Set("STATUS_ERROR_FOREGROUND", "160")
	c.Set("PROMPT_CHAR_OK_FOREGROUND", "76")
	c.Set("PROMPT_CHAR_ERROR_FOREGROUND", "196")
	c.Set("COMMAND_EXECUTION_TIME_FOREGROUND", "101")
	c.Set("BACKGROUND_JOBS_FOREGROUND", "70")
	c.Set("CONTEXT_FOREGROUND", "180")
	c.Set("TIME_FOREGROUND", "66")
	return c
}

// presetLean8 is lean restricted to the terminal's own eight colors,
// for the terminals and color schemes where the 256-color palette
// lands somewhere unreadable.
func presetLean8() *Config {
	c := baseConfig()
	leanShape(c)
	c.Set("DIR_FOREGROUND", "blue")
	c.Set("DIR_SHORTENED_FOREGROUND", "blue")
	c.Set("DIR_ANCHOR_FOREGROUND", "bright-blue")
	c.Set("DIR_ANCHOR_BOLD", "true")
	c.Set("VCS_CLEAN_FOREGROUND", "green")
	c.Set("VCS_UNTRACKED_FOREGROUND", "green")
	c.Set("VCS_MODIFIED_FOREGROUND", "yellow")
	c.Set("VCS_CONFLICTED_FOREGROUND", "red")
	c.Set("STATUS_OK_FOREGROUND", "green")
	c.Set("STATUS_ERROR_FOREGROUND", "red")
	c.Set("PROMPT_CHAR_OK_FOREGROUND", "green")
	c.Set("PROMPT_CHAR_ERROR_FOREGROUND", "red")
	c.Set("COMMAND_EXECUTION_TIME_FOREGROUND", "yellow")
	c.Set("BACKGROUND_JOBS_FOREGROUND", "green")
	c.Set("CONTEXT_FOREGROUND", "yellow")
	c.Set("TIME_FOREGROUND", "cyan")
	return c
}

// presetClassic is the boxed, powerline look on a single muted
// background — the shape most people picture when they picture p10k.
func presetClassic() *Config {
	c := baseConfig()
	powerlineShape(c)
	frame(c, "242")
	c.Set("BACKGROUND", "238")
	c.Set("SUBSEGMENT_SEPARATOR_FOREGROUND", "244")
	c.Set("DIR_FOREGROUND", "31")
	c.Set("DIR_SHORTENED_FOREGROUND", "103")
	c.Set("DIR_ANCHOR_FOREGROUND", "39")
	c.Set("DIR_ANCHOR_BOLD", "true")
	c.Set("VCS_CLEAN_FOREGROUND", "76")
	c.Set("VCS_UNTRACKED_FOREGROUND", "76")
	c.Set("VCS_MODIFIED_FOREGROUND", "178")
	c.Set("VCS_CONFLICTED_FOREGROUND", "196")
	c.Set("STATUS_OK_FOREGROUND", "70")
	c.Set("STATUS_ERROR_FOREGROUND", "160")
	c.Set("PROMPT_CHAR_OK_FOREGROUND", "76")
	c.Set("PROMPT_CHAR_ERROR_FOREGROUND", "196")
	c.Set("COMMAND_EXECUTION_TIME_FOREGROUND", "101")
	c.Set("BACKGROUND_JOBS_FOREGROUND", "70")
	c.Set("CONTEXT_FOREGROUND", "180")
	c.Set("TIME_FOREGROUND", "66")
	return c
}

// presetRainbow gives each segment its own background, so the prompt
// reads as a ribbon of color. It is the most font- and palette-
// dependent of the presets, and the most striking when it lands.
func presetRainbow() *Config {
	c := baseConfig()
	powerlineShape(c)
	frame(c, "242")
	c.Set("BACKGROUND", "238")
	c.Set("SUBSEGMENT_SEPARATOR_FOREGROUND", "180")

	c.Set("DIR_BACKGROUND", "4")
	c.Set("DIR_FOREGROUND", "254")
	c.Set("DIR_SHORTENED_FOREGROUND", "250")
	c.Set("DIR_ANCHOR_FOREGROUND", "255")
	c.Set("DIR_ANCHOR_BOLD", "true")

	c.Set("VCS_CLEAN_BACKGROUND", "2")
	c.Set("VCS_MODIFIED_BACKGROUND", "3")
	c.Set("VCS_UNTRACKED_BACKGROUND", "2")
	c.Set("VCS_CONFLICTED_BACKGROUND", "3")
	c.Set("VCS_CLEAN_FOREGROUND", "0")
	c.Set("VCS_MODIFIED_FOREGROUND", "0")
	c.Set("VCS_UNTRACKED_FOREGROUND", "0")
	c.Set("VCS_CONFLICTED_FOREGROUND", "0")

	c.Set("STATUS_OK_BACKGROUND", "2")
	c.Set("STATUS_OK_FOREGROUND", "0")
	c.Set("STATUS_ERROR_BACKGROUND", "1")
	c.Set("STATUS_ERROR_FOREGROUND", "0")
	c.Set("COMMAND_EXECUTION_TIME_BACKGROUND", "1")
	c.Set("COMMAND_EXECUTION_TIME_FOREGROUND", "0")
	c.Set("BACKGROUND_JOBS_BACKGROUND", "1")
	c.Set("BACKGROUND_JOBS_FOREGROUND", "0")
	c.Set("CONTEXT_BACKGROUND", "3")
	c.Set("CONTEXT_FOREGROUND", "0")
	c.Set("TIME_BACKGROUND", "7")
	c.Set("TIME_FOREGROUND", "0")
	c.Set("PROMPT_CHAR_OK_FOREGROUND", "76")
	c.Set("PROMPT_CHAR_ERROR_FOREGROUND", "196")
	return c
}

// presetPure reproduces the Pure theme's restraint: two lines, almost
// no color, and nothing on the right but the timing.
func presetPure() *Config {
	c := baseConfig()
	leanShape(c)
	c.SetList("LEFT_PROMPT_ELEMENTS", []string{"context", "dir", "vcs", elementNewline, "prompt_char"})
	c.SetList("RIGHT_PROMPT_ELEMENTS", []string{"command_execution_time"})
	c.Set("DIR_FOREGROUND", "blue")
	c.Set("DIR_ANCHOR_BOLD", "false")
	c.Set("VCS_CLEAN_FOREGROUND", "242")
	c.Set("VCS_MODIFIED_FOREGROUND", "242")
	c.Set("VCS_UNTRACKED_FOREGROUND", "242")
	c.Set("VCS_CONFLICTED_FOREGROUND", "red")
	c.Set("PROMPT_CHAR_OK_FOREGROUND", "magenta")
	c.Set("PROMPT_CHAR_ERROR_FOREGROUND", "red")
	c.Set("PROMPT_CHAR_OK_CONTENT_EXPANSION", "❯")
	c.Set("PROMPT_CHAR_ERROR_CONTENT_EXPANSION", "❯")
	c.Set("COMMAND_EXECUTION_TIME_FOREGROUND", "yellow")
	c.Set("COMMAND_EXECUTION_TIME_THRESHOLD", "5")
	c.Set("CONTEXT_FOREGROUND", "242")
	c.Set("CONTEXT_ALWAYS_SHOW", "true")
	c.Set("CONTEXT_TEMPLATE", "%n@%m")
	c.Set("PROMPT_ADD_NEWLINE", "true")
	return c
}

// presetRobbyRussell is the oh-my-zsh default that most people are
// actually leaving behind, kept so the move can be made in one step
// without changing how the prompt looks at all.
func presetRobbyRussell() *Config {
	c := baseConfig()
	leanShape(c)
	c.SetList("LEFT_PROMPT_ELEMENTS", []string{"prompt_char", "dir", "vcs"})
	c.SetList("RIGHT_PROMPT_ELEMENTS", nil)
	c.Set("PROMPT_ADD_NEWLINE", "false")
	c.Set("PROMPT_CHAR_OK_CONTENT_EXPANSION", "➜")
	c.Set("PROMPT_CHAR_ERROR_CONTENT_EXPANSION", "➜")
	c.Set("PROMPT_CHAR_OK_FOREGROUND", "green")
	c.Set("PROMPT_CHAR_ERROR_FOREGROUND", "red")
	c.Set("DIR_FOREGROUND", "cyan")
	c.Set("DIR_ANCHOR_BOLD", "true")
	c.Set("SHORTEN_STRATEGY", "truncate_to_last")
	c.Set("VCS_CLEAN_FOREGROUND", "blue")
	c.Set("VCS_MODIFIED_FOREGROUND", "yellow")
	c.Set("VCS_UNTRACKED_FOREGROUND", "blue")
	c.Set("VCS_CONFLICTED_FOREGROUND", "red")
	c.Set("VCS_BRANCH_ICON", "git:(")
	c.Set("VCS_BRANCH_SUFFIX", ")")
	return c
}
