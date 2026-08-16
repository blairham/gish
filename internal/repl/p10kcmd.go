package repl

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/p10k"
	"github.com/blairham/gish/internal/term"
)

// The p10k command: the configuration surface for the native engine.
//
// `p10k configure` is the wizard, and it follows upstream's shape
// because upstream got it right: ask one question at a time, show the
// answer rendered rather than described, and never ask the terminal
// something the human can see better than we can detect (whether a glyph
// rendered is a font question, so it is asked, not sniffed).
//
// Unlike upstream, the wizard writes a *complete* configuration — the
// chosen preset resolved into explicit settings — so the file is the
// whole answer and later edits are local and greppable.

const p10kUsage = `usage: p10k [configure | import [path] | show | preset <name> | list]

  p10k configure        the wizard: pick a look, preview it, save it
  p10k import [path]    take the settings from a .p10k.zsh (default ~/.p10k.zsh)
  p10k show             the resolved configuration and where it came from
  p10k preset <name>    switch to a preset without the wizard
  p10k list             available presets and segments`

// p10kCallHandler intercepts `p10k`, config-style: it edits the config
// file and takes effect in the same breath.
func p10kCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "p10k" {
			return next(ctx, args)
		}
		return runP10k(interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runP10k(hc interp.HandlerContext, args []string) []string {
	fail := func(err error) []string {
		fmt.Fprintln(hc.Stderr, "p10k:", err)
		return []string{"false"}
	}

	switch {
	case len(args) == 0, args[0] == "show":
		showP10k(hc)
		return []string{"true"}

	case args[0] == "help", args[0] == "-h", args[0] == "--help":
		fmt.Fprintln(hc.Stdout, p10kUsage)
		return []string{"true"}

	case args[0] == "list":
		fmt.Fprintf(hc.Stdout, "presets:  %s\n", strings.Join(p10k.Presets(), " "))
		fmt.Fprintf(hc.Stdout, "segments: %s\n", strings.Join(p10k.Segments(), " "))
		return []string{"true"}

	case args[0] == "preset" && len(args) == 2:
		cfg := p10k.Preset(args[1])
		if cfg == nil {
			return fail(fmt.Errorf("no preset %q — try one of: %s", args[1], strings.Join(p10k.Presets(), " ")))
		}
		path, err := p10k.SaveNativeConfig(cfg)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(hc.Stdout, "saved %s to %s\n", args[1], displayPath(path))
		return p10kActivate(hc)

	case args[0] == "import":
		return importP10k(hc, args[1:])

	case args[0] == "configure":
		return configureP10k(hc)
	}

	fmt.Fprintln(hc.Stderr, p10kUsage)
	return []string{"false"}
}

// showP10k reports what is in effect and where each layer came from.
func showP10k(hc interp.HandlerContext) {
	cfg := p10kConfigFromEnv(hc.Env)
	fmt.Fprintf(hc.Stdout, "theme      %s\n", hc.Env.Get("GISH_THEME").String())
	fmt.Fprintf(hc.Stdout, "preset     %s\n", p10kPresetName(hc))
	if path, err := p10k.ConfigPath(); err == nil {
		state := "not written yet"
		if _, statErr := os.Stat(path); statErr == nil {
			state = "in use"
		}
		fmt.Fprintf(hc.Stdout, "config     %s (%s)\n", displayPath(path), state)
	}
	fmt.Fprintf(hc.Stdout, "left       %s\n", strings.Join(cfg.List("LEFT_PROMPT_ELEMENTS"), " "))
	fmt.Fprintf(hc.Stdout, "right      %s\n", strings.Join(cfg.List("RIGHT_PROMPT_ELEMENTS"), " "))

	// Elements with no implementation would otherwise just be invisible.
	var missing []string
	for _, side := range []string{"LEFT_PROMPT_ELEMENTS", "RIGHT_PROMPT_ELEMENTS"} {
		for _, e := range cfg.List(side) {
			if !p10k.Known(e) && !slices.Contains(missing, e) {
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
	if v := cfg.Str("INSTANT_PROMPT", "off"); v != "off" && v != "" {
		fmt.Fprintf(hc.Stdout, "note       %s\n", instantPromptNote)
	}
}

func p10kPresetName(hc interp.HandlerContext) string {
	if v := hc.Env.Get("GISH_P10K_PRESET").String(); v != "" {
		return v
	}
	return p10k.DefaultPreset
}

// importP10k takes the settings out of a .p10k.zsh, once.
func importP10k(hc interp.HandlerContext, args []string) []string {
	path := ""
	switch len(args) {
	case 0:
		p, err := p10k.DefaultZshConfigPath()
		if err != nil {
			fmt.Fprintln(hc.Stderr, "p10k:", err)
			return []string{"false"}
		}
		path = p
	case 1:
		path = args[0]
	default:
		fmt.Fprintln(hc.Stderr, p10kUsage)
		return []string{"false"}
	}

	imported, err := p10k.ImportZshConfig(path)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "p10k:", err)
		return []string{"false"}
	}

	// The preset underneath supplies anything the file did not mention,
	// so an import never leaves a half-configured prompt.
	cfg := p10k.Preset(p10k.DefaultPreset)
	cfg.Merge(imported)
	saved, err := p10k.SaveNativeConfig(cfg)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "p10k:", err)
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
	return p10kActivate(hc)
}

// p10kActivate turns the theme on for this session and persists it, so
// configuring the prompt is one step rather than "now also set a
// variable".
func p10kActivate(hc interp.HandlerContext) []string {
	if hc.Env.Get("GISH_THEME").String() == "p10k" {
		return []string{"true"}
	}
	assigns, ok := persistPairs(hc, [][2]string{{"GISH_THEME", "p10k"}})
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

func configureP10k(hc interp.HandlerContext) []string {
	choose := p10kChooser(hc)
	if choose == nil {
		fmt.Fprintln(hc.Stderr, "p10k configure needs a terminal; use `p10k preset <name>` instead")
		return []string{"false"}
	}

	fmt.Fprintln(hc.Stdout, "gish p10k configuration — Ctrl-C aborts, nothing is saved until the end.")

	preset, ok := choose("Which look?", presetOptions())
	if !ok {
		return p10kAbort(hc)
	}
	cfg := p10k.Preset(preset)
	if cfg == nil {
		cfg = p10k.Preset(p10k.DefaultPreset)
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
	fmt.Fprintln(hc.Stdout, p10k.Render(cfg, p10kPreviewContext()).Prompt)
	fmt.Fprintln(hc.Stdout)

	confirm, ok := choose("Save this?", []chooseOption{
		{"y", "save and switch to it"},
		{"n", "discard — change nothing"},
	})
	if !ok || confirm == "n" {
		return p10kAbort(hc)
	}

	path, err := p10k.SaveNativeConfig(cfg)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "p10k:", err)
		return []string{"false"}
	}
	fmt.Fprintf(hc.Stdout, "saved %s\n", displayPath(path))
	return p10kActivate(hc)
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
// implement, for `p10k show` to print when a config asks for it.
//
// Upstream's instant prompt paints a cached prompt at startup because
// the real one is not ready for tens of milliseconds — zsh has to load
// the framework before it can render anything. gish measures 7ms from
// exec to a fully resolved p10k prompt, which is the same number it
// measures for the naked one (cmd/gish/startup_p10k_test.go). There is
// nothing to hide behind a cache.
//
// So the setting is accepted and ignored rather than implemented. A
// prompt cache is not free: it has to be invalidated, it goes stale
// across directory changes, and upstream's version is well known for the
// console-output warnings it produces when something prints during
// startup. Carrying that to solve a problem this shell does not have
// would be the wrong kind of faithful.
const instantPromptNote = "INSTANT_PROMPT is accepted but not needed: " +
	"gish resolves the real prompt in ~7ms at startup, so there is nothing to cache ahead of it"

// asciiOnly strips everything that needs a patched font, so the prompt
// is plain text end to end.
func asciiOnly(cfg *p10k.Config) {
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
	cfg.Set("STATUS_ERROR_VISUAL_IDENTIFIER_EXPANSION", "x")
	cfg.Set("STATUS_OK_VISUAL_IDENTIFIER_EXPANSION", "+")
	cfg.Set("BACKGROUND_JOBS_VISUAL_IDENTIFIER_EXPANSION", "%")
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
	for _, name := range p10k.Presets() {
		opts = append(opts, chooseOption{name, name + " — " + described[name]})
	}
	return opts
}

// p10kPreviewContext is the canned state the preview renders: realistic
// enough to judge a layout, and touching nothing real.
func p10kPreviewContext() *p10k.Context {
	home, _ := os.UserHomeDir()
	return &p10k.Context{
		Cwd:      home + "/dev/gish",
		Home:     home,
		Username: "you",
		Hostname: "host",
		ExitCode: 1,
		Duration: 4 * time.Second,
		Jobs:     1,
		Width:    previewWidth(),
		Now:      time.Now(),
		Git: &p10k.GitStatus{
			Dir: home + "/dev/gish", Branch: "main",
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
