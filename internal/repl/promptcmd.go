package repl

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/promptengine"
	"github.com/blairham/koi-shell/internal/term"
)

// The prompt command: the configuration surface for the theme engine.
//
// `prompt configure` is the wizard, and it follows powerlevel10k's shape
// because upstream got it right: ask one question at a time, show the
// answer rendered rather than described, and never ask the terminal
// something the human can see better than we can detect (whether a glyph
// rendered is a font question, so it is asked, not sniffed).
//
// Unlike upstream, the wizard writes a *complete* configuration — the
// chosen preset resolved into explicit settings — so the file is the
// whole answer and later edits are local and greppable.
//
// The command is named for what it configures rather than for one of the
// dialects that feed it (#184). The engine renders presets from several
// upstreams, and `p10k preset tokyo-night` would claim powerlevel10k
// ships a look it does not. `p10k` remains an alias, because it is what
// arrivals type from muscle memory — the settings namespace it names
// (POWERLEVEL9K_*) is unchanged, and #134 settled that it stays.

// promptCmdNames are the spellings that reach this command: the name,
// then the compatibility alias.
var promptCmdNames = []string{"prompt", "p10k"}

// promptUsage is spelled with whichever name was invoked, so help for
// `p10k` does not answer in a vocabulary the user did not type.
func promptUsage(name string) string {
	return fmt.Sprintf(`usage: %[1]s [configure | import [path] | show | preset <name> | list]

  %[1]s configure        the wizard: pick a look, preview it, save it
  %[1]s import [path]    take the settings from a .p10k.zsh (default ~/.p10k.zsh)
  %[1]s show             the resolved configuration and where it came from
  %[1]s preset <name>    switch to a preset without the wizard
  %[1]s list             available presets and segments`, name)
}

// promptCallHandler intercepts `prompt` and its `p10k` alias,
// config-style: it edits the config file and takes effect in the same
// breath.
func promptCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if !slices.Contains(promptCmdNames, args[0]) {
			return next(ctx, args)
		}
		return runPrompt(interp.HandlerCtx(ctx), args[0], args[1:]), nil
	}
}

func runPrompt(hc interp.HandlerContext, name string, args []string) []string {
	fail := func(err error) []string {
		fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
		return []string{"false"}
	}

	switch {
	case len(args) == 0, args[0] == "show":
		showPrompt(hc)
		return []string{"true"}

	case args[0] == "help", args[0] == "-h", args[0] == "--help":
		fmt.Fprintln(hc.Stdout, promptUsage(name))
		return []string{"true"}

	case args[0] == "list":
		fmt.Fprintf(hc.Stdout, "presets:  %s\n", strings.Join(promptengine.Presets(), " "))
		fmt.Fprintf(hc.Stdout, "segments: %s\n", strings.Join(promptengine.Segments(), " "))
		return []string{"true"}

	case args[0] == "preset" && len(args) == 2:
		cfg := promptengine.Preset(args[1])
		if cfg == nil {
			return fail(fmt.Errorf("no preset %q — try one of: %s", args[1], strings.Join(promptengine.Presets(), " ")))
		}
		path, err := promptengine.SaveNativeConfig(cfg)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(hc.Stdout, "saved %s to %s\n", args[1], displayPath(path))
		// Say what the look drops, here and now. The saved file is a
		// resolved list of settings, so this metadata does not survive
		// the round trip — `prompt show` reads the file back and cannot
		// know what the preset chose not to carry. Choosing the preset
		// is the moment the user can act on it anyway.
		reportPresetGaps(hc, args[1], cfg)
		return promptActivate(hc)

	case args[0] == "import":
		return importP10k(hc, name, args[1:])

	case args[0] == "configure":
		return configurePrompt(hc, name)
	}

	fmt.Fprintln(hc.Stderr, promptUsage(name))
	return []string{"false"}
}

// reportPresetGaps names what a preset does not reproduce, in the
// vocabulary of the project the look came from.
//
// A preset ported from another prompt is either faithful or it says
// where it is not, which is the same rule `prompt import` follows when
// it names the settings whose value was shell code. Silence here would
// be worse than in the import case: someone who picked a look off a
// screenshot has the screenshot in front of them, and would otherwise
// spend the evening deciding they had configured it wrong.
func reportPresetGaps(hc interp.HandlerContext, name string, cfg *promptengine.Config) {
	if len(cfg.Unsupported) == 0 {
		return
	}
	fmt.Fprintf(hc.Stdout, "\n%s does not reproduce everything its upstream shows:\n", name)
	for _, u := range cfg.Unsupported {
		fmt.Fprintf(hc.Stdout, "  %s\n", u)
	}
}

// showPrompt reports what is in effect and where each layer came from.
func showPrompt(hc interp.HandlerContext) {
	cfg := p10kConfigFromEnv(hc.Env)
	fmt.Fprintf(hc.Stdout, "theme      %s\n", hc.Env.Get("KOI_THEME").String())
	fmt.Fprintf(hc.Stdout, "preset     %s\n", p10kPresetName(hc))
	if path, err := promptengine.ConfigPath(); err == nil {
		state := "not written yet"
		if _, statErr := os.Stat(path); statErr == nil {
			state = "in use"
		}
		fmt.Fprintf(hc.Stdout, "config     %s (%s)\n", displayPath(path), state)
	}
	// Which icon set is actually serving (#131). A MODE koi does not
	// carry is served by nerdfont-v3 and says so here: silently
	// substituting a glyph set is how a prompt ends up full of boxes
	// with no explanation.
	if mode := cfg.ResolveIconMode(); mode.Fallback() {
		fmt.Fprintf(hc.Stdout, "icons      %s (asked for %s, which koi does not carry)\n", mode.Serving, mode.Requested)
	} else {
		fmt.Fprintf(hc.Stdout, "icons      %s\n", mode.Serving)
	}
	fmt.Fprintf(hc.Stdout, "left       %s\n", strings.Join(cfg.List("LEFT_PROMPT_ELEMENTS"), " "))
	fmt.Fprintf(hc.Stdout, "right      %s\n", strings.Join(cfg.List("RIGHT_PROMPT_ELEMENTS"), " "))

	// Elements with no implementation would otherwise just be invisible.
	var missing []string
	for _, side := range []string{"LEFT_PROMPT_ELEMENTS", "RIGHT_PROMPT_ELEMENTS"} {
		for _, e := range cfg.List(side) {
			if !promptengine.Known(e) && !slices.Contains(missing, e) {
				missing = append(missing, e)
			}
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(hc.Stdout, "not yet    %s\n", strings.Join(missing, " "))
	}
	for _, u := range cfg.Unsupported {
		fmt.Fprintf(hc.Stdout, "skipped    %s\n", u)
	}
	// Set and stored, but not acted on (#133). Without this line the
	// state is invisible: the setting survives a round trip and the
	// prompt quietly does something else.
	for _, u := range cfg.UnhonouredSettings() {
		fmt.Fprintf(hc.Stdout, "ignored    %s\n", u)
	}
	if v := cfg.Str("INSTANT_PROMPT", "off"); v != "off" && v != "" {
		fmt.Fprintf(hc.Stdout, "note       %s\n", instantPromptNote)
	}
}

func p10kPresetName(hc interp.HandlerContext) string {
	if v := hc.Env.Get("KOI_P10K_PRESET").String(); v != "" {
		return v
	}
	return promptengine.DefaultPreset
}

// importP10k takes the settings out of a .p10k.zsh, once. The name is
// still p10k's: this reads powerlevel10k's own dialect, whatever the
// command that reached it was spelled.
func importP10k(hc interp.HandlerContext, name string, args []string) []string {
	path := ""
	switch len(args) {
	case 0:
		p, err := promptengine.DefaultZshConfigPath()
		if err != nil {
			fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
			return []string{"false"}
		}
		path = p
	case 1:
		path = args[0]
	default:
		fmt.Fprintln(hc.Stderr, promptUsage(name))
		return []string{"false"}
	}

	imported, err := promptengine.ImportZshConfig(path)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
		return []string{"false"}
	}

	// The preset underneath supplies anything the file did not mention,
	// so an import never leaves a half-configured prompt.
	cfg := promptengine.Preset(promptengine.DefaultPreset)
	cfg.Merge(imported)
	saved, err := promptengine.SaveNativeConfig(cfg)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
		return []string{"false"}
	}

	fmt.Fprintf(hc.Stdout, "imported %d settings from %s\n", len(imported.Keys()), displayPath(path))
	fmt.Fprintf(hc.Stdout, "wrote %s\n", displayPath(saved))
	if len(imported.Unsupported) > 0 {
		// Say exactly what did not come across. These are the settings
		// whose value is shell code, and pretending otherwise would show
		// a prompt that is subtly not the one the user configured.
		fmt.Fprintf(hc.Stdout, "\n%d settings could not be taken — their values are shell code, not data:\n",
			len(imported.Unsupported))
		for _, u := range imported.Unsupported {
			fmt.Fprintf(hc.Stdout, "  %s\n", u)
		}
		fmt.Fprintln(hc.Stdout, "\nThe native segments cover these; nothing else is needed unless you had customized them.")
	}
	return promptActivate(hc)
}

// promptActivate turns the theme on for this session and persists it, so
// configuring the prompt is one step rather than "now also set a
// variable". KOI_THEME stays spelled "p10k": #184 renamed the command,
// not the theme value, and #134 keeps the dialect names honest.
func promptActivate(hc interp.HandlerContext) []string {
	if hc.Env.Get("KOI_THEME").String() == "p10k" {
		return []string{"true"}
	}
	assigns, ok := persistPairs(hc, [][2]string{{"KOI_THEME", "p10k"}})
	if !ok {
		return []string{"false"}
	}
	return append([]string{"eval"}, strings.Join(assigns, " "))
}

// ------------------------------------------------------------ wizard

// p10kQuestion is one step of the wizard: a config key, a prompt, and
// the answers, each of which knows how to describe itself.
type p10kQuestion struct {
	key     string // config key the answer sets
	prompt  string
	options []chooseOption
}

func configurePrompt(hc interp.HandlerContext, name string) []string {
	choose := p10kChooser(hc)
	if choose == nil {
		fmt.Fprintf(hc.Stderr, "%[1]s configure needs a terminal; use `%[1]s preset <name>` instead\n", name)
		return []string{"false"}
	}

	fmt.Fprintln(hc.Stdout, "koi prompt configuration — Ctrl-C aborts, nothing is saved until the end.")

	preset, ok := choose("Which look?", presetOptions())
	if !ok {
		return p10kAbort(hc)
	}
	cfg := promptengine.Preset(preset)
	if cfg == nil {
		cfg = promptengine.Preset(promptengine.DefaultPreset)
	}

	// Glyph support is a font question. Show the glyphs and let the
	// person looking at them answer — detection here is guesswork, and
	// wrong guesses produce the boxes-and-question-marks prompt that
	// makes people give up on a theme entirely.
	fmt.Fprintf(hc.Stdout, "\n  Powerline:        Icons:    \n\n")
	glyphs, ok := choose("Did those render as arrows and icons, or as boxes?", []chooseOption{
		{"y", "they rendered — use powerline separators and icons"},
		{"n", "boxes or blanks — keep it to plain text"},
	})
	if !ok {
		return p10kAbort(hc)
	}
	if glyphs == "n" {
		asciiOnly(cfg)
	}

	for _, q := range p10kQuestions() {
		answer, qok := choose(q.prompt, q.options)
		if !qok {
			return p10kAbort(hc)
		}
		cfg.Set(q.key, answer)
	}

	// Show the result rather than describing it.
	fmt.Fprintln(hc.Stdout, "\npreview:")
	fmt.Fprintln(hc.Stdout, promptengine.Render(cfg, p10kPreviewContext()).Prompt)
	fmt.Fprintln(hc.Stdout)

	confirm, ok := choose("Save this?", []chooseOption{
		{"y", "save and switch to it"},
		{"n", "discard — change nothing"},
	})
	if !ok || confirm == "n" {
		return p10kAbort(hc)
	}

	path, err := promptengine.SaveNativeConfig(cfg)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "%s: %v\n", name, err)
		return []string{"false"}
	}
	fmt.Fprintf(hc.Stdout, "saved %s\n", displayPath(path))
	return promptActivate(hc)
}

// p10kQuestions is the walkthrough after the look has been chosen. Each
// one is a setting someone would otherwise have to find in a file.
func p10kQuestions() []p10kQuestion {
	return []p10kQuestion{
		{
			key:    "PROMPT_ADD_NEWLINE",
			prompt: "Blank line between commands?",
			options: []chooseOption{
				{"true", "yes — the prompt gets room to breathe"},
				{"false", "no — compact"},
			},
		},
		{
			key:    "TRANSIENT_PROMPT",
			prompt: "Trim old prompts once a command has run?",
			options: []chooseOption{
				{"always", "yes — past prompts collapse to the bare character"},
				{"same-dir", "only while staying in the same directory"},
				{"off", "no — leave them as they were"},
			},
		},
	}
	// There is deliberately no INSTANT_PROMPT question. See instantPromptNote.
}

// instantPromptNote explains the one upstream feature this port does not
// implement, for `prompt show` to print when a config asks for it.
//
// Upstream's instant prompt paints a cached prompt at startup because
// the real one is not ready for tens of milliseconds — zsh has to load
// the framework before it can render anything. koi measures 7ms from
// exec to a fully resolved p10k prompt, which is the same number it
// measures for the naked one (cmd/koi/startup_p10k_test.go). There is
// nothing to hide behind a cache.
//
// So the setting is accepted and ignored rather than implemented. A
// prompt cache is not free: it has to be invalidated, it goes stale
// across directory changes, and upstream's version is well known for the
// console-output warnings it produces when something prints during
// startup. Carrying that to solve a problem this shell does not have
// would be the wrong kind of faithful.
const instantPromptNote = "INSTANT_PROMPT is accepted but not needed: " +
	"koi resolves the real prompt in ~7ms at startup, so there is nothing to cache ahead of it"

// asciiOnly strips everything that needs a patched font, so the prompt
// is plain text end to end.
//
// Since the icon table landed (#131) the MODE below does the icons on
// its own, so what is left here is the *separators* and prompt
// characters — which are layout, not icons, and are not in the table.
// The per-segment icon overrides this used to write are gone: they were
// the workaround for MODE selecting nothing.
func asciiOnly(cfg *promptengine.Config) {
	cfg.Set("MODE", "ascii")
	for _, side := range []string{"LEFT", "RIGHT"} {
		cfg.Set(side+"_SEGMENT_SEPARATOR", "")
		cfg.Set(side+"_SUBSEGMENT_SEPARATOR", " ")
	}
	cfg.Set("LEFT_PROMPT_LAST_SEGMENT_END_SYMBOL", "")
	cfg.Set("RIGHT_PROMPT_FIRST_SEGMENT_START_SYMBOL", "")
	cfg.Set("MULTILINE_FIRST_PROMPT_PREFIX", "")
	cfg.Set("MULTILINE_NEWLINE_PROMPT_PREFIX", "")
	cfg.Set("MULTILINE_LAST_PROMPT_PREFIX", "")
	cfg.Set("MULTILINE_FIRST_PROMPT_SUFFIX", "")
	cfg.Set("MULTILINE_NEWLINE_PROMPT_SUFFIX", "")
	cfg.Set("MULTILINE_LAST_PROMPT_SUFFIX", "")
	cfg.Set("PROMPT_CHAR_OK_CONTENT_EXPANSION", ">")
	cfg.Set("PROMPT_CHAR_ERROR_CONTENT_EXPANSION", ">")
}

func presetOptions() []chooseOption {
	described := map[string]string{
		"lean":         "two lines, no backgrounds — the default, and font-proof",
		"classic":      "framed, one muted background, powerline separators",
		"rainbow":      "framed, a background per segment — the loud one",
		"pure":         "the Pure theme's restraint: almost no color",
		"lean-8colors": "lean, restricted to the terminal's own eight colors",
		"robbyrussell": "the oh-my-zsh default, one line, unchanged",
	}
	opts := make([]chooseOption, 0, len(described))
	for _, name := range promptengine.Presets() {
		opts = append(opts, chooseOption{name, name + " — " + described[name]})
	}
	return opts
}

// p10kPreviewContext is the canned state the preview renders: realistic
// enough to judge a layout, and touching nothing real.
func p10kPreviewContext() *promptengine.Context {
	home, _ := os.UserHomeDir()
	return &promptengine.Context{
		Cwd:      home + "/dev/koi",
		Home:     home,
		Username: "you",
		Hostname: "host",
		ExitCode: 1,
		Duration: 4 * time.Second,
		Jobs:     1,
		Width:    previewWidth(),
		Now:      time.Now(),
		Git: &promptengine.GitStatus{
			Dir: home + "/dev/koi", Branch: "main",
			Ahead: 2, Modified: 3, Untracked: 1,
		},
		Getenv: func(string) string { return "" },
	}
}

// previewWidth is the terminal width the preview is laid out for,
// falling back to the classic 80 when there is no terminal to ask.
func previewWidth() int {
	if w, _, err := term.NewTTY(os.Stdin, os.Stdout).Size(); err == nil && w > 0 {
		return w
	}
	return 80
}

func p10kAbort(hc interp.HandlerContext) []string {
	fmt.Fprintln(hc.Stdout, "nothing saved")
	return []string{"true"}
}

// p10kChooser is the interactive select when there is a terminal, and a
// plain line reader otherwise, so the same wizard drives both a real
// session and a test.
func p10kChooser(hc interp.HandlerContext) chooser {
	if c := interactiveChooser(hc.Stdin, hc.Stdout); c != nil {
		return c
	}
	if !stdinIsTTY(hc.Stdin) {
		return lineChooser(hc)
	}
	return nil
}

// lineChooser asks by printing the options and reading a key, which is
// what tests and piped input use. The keys are the same ones the select
// returns, so both frontends speak one vocabulary.
func lineChooser(hc interp.HandlerContext) chooser {
	in := bufio.NewScanner(hc.Stdin)
	return func(prompt string, options []chooseOption) (string, bool) {
		for {
			fmt.Fprintln(hc.Stdout, prompt)
			for _, o := range options {
				fmt.Fprintf(hc.Stdout, "  [%s] %s\n", o.key, o.label)
			}
			fmt.Fprint(hc.Stdout, "> ")
			if !in.Scan() {
				fmt.Fprintln(hc.Stdout)
				return "", false
			}
			answer := strings.TrimSpace(in.Text())
			for _, o := range options {
				if answer == o.key {
					return o.key, true
				}
			}
			fmt.Fprintf(hc.Stdout, "  pick one of the keys above\n")
		}
	}
}
