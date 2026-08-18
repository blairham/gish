package repl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/koi-shell/internal/promptengine"
	"github.com/blairham/koi-shell/internal/tools"
)

// Themes. koi starts naked: the out-of-box prompt is the familiar
// zsh/bash shape (user@host dir %) with no color and no branding —
// switching shells shouldn't change how the terminal greets you until
// you ask it to. The p10k-class two-line prompt (#26, built natively on
// the segment engine — async plugin segments, budget deadlines,
// stale-serve) and starship (#45) are explicit opt-ins.
//
// Precedence: a user-set KOI_PROMPT always wins (the bare-metal escape
// hatch); otherwise KOI_THEME selects "p10k", "starship", or the naked
// default. Dumb terminals and NO_COLOR get the naked prompt regardless.

const (
	cReset = "\x1b[0m"
	cDim   = "\x1b[2m"
	cCyan  = "\x1b[36m"
	cMag   = "\x1b[35m"
	cRed   = "\x1b[31m"
	cGreen = "\x1b[32m"
	cYel   = "\x1b[33m"
)

// builtinThemes maps the reserved names to in-tree renderers. In-tree
// and plugin themes are peers behind the same signature (#30) — a theme
// turns the prompt state into the full (prompt, cont, rprompt) set —
// but these names cannot be claimed by plugins.
var builtinThemes = map[string]func(*interp.Runner, promptInfo) (string, string, string){
	"plain": func(_ *interp.Runner, info promptInfo) (string, string, string) {
		p, cp := nakedPrompt(info)
		return p, cp, ""
	},
	// p10k is the native powerlevel10k engine (internal/promptengine): the
	// presets, the segment set and the layout, rendered in-process.
	"p10k": p10kTheme,
	// koi is this shell's own segment-knob theme — the one KOI_THEME_*
	// configures. It predates the p10k engine and stays because those
	// knobs are a documented surface, and because it is the fallback
	// when a named theme cannot render.
	"koi": nativeTheme,
	// literal: the KOI_PROMPT override, rendered as a theme so there is
	// exactly one prompt pipeline (#109).
	"literal": func(runner *interp.Runner, info promptInfo) (string, string, string) {
		return expandPrompt(shellVar(runner, "KOI_PROMPT", ""), info),
			expandPrompt(shellVar(runner, "KOI_PROMPT_CONT", contPrompt), info), ""
	},
	// bash: whatever the session set PS1 to (#159). Not a look of its
	// own — it is how starship, oh-my-posh and a hand-written PS1 get
	// rendered at all.
	"bash": ps1Theme,
	"starship": func(runner *interp.Runner, info promptInfo) (string, string, string) {
		if p, cp, ok := starship.render(info, info.width); ok {
			return p, cp, ""
		}
		return nativeTheme(runner, info) // missing binary: native fallback
	},
}

// nativeTheme is the built-in p10k-class theme: the great default every
// other theme is measured against, and the fallback when a selected
// theme cannot render.
func nativeTheme(runner *interp.Runner, info promptInfo) (string, string, string) {
	cfg := themeConfigFrom(runner)
	p, cp := themedPrompt(info, cfg)
	return p, cp, themedRPrompt(info, cfg)
}

// promptStrings resolves the full prompt set for the next read.
// Precedence: manual KOI_PROMPT > KOI_THEME — a built-in name, else a
// discovered plugin theme claiming it (budget-bounded, stale on miss),
// else the native theme > plain when nothing was asked for. NO_COLOR
// and dumb terminals degrade to naked regardless of source.
func promptStrings(runner *interp.Runner, info promptInfo) (prompt, cont, rprompt string) {
	name := themeName(runner)
	if theme, ok := builtinThemes[name]; ok {
		return theme(runner, info)
	}
	if themePlugins != nil {
		if set, ok := themePlugins.render(context.Background(), name, info); ok {
			return set.prompt, set.cont, set.rprompt
		}
	}
	// A theme was asked for by name but nothing serves it: the native
	// theme beats silently going naked (doctor explains the why).
	return nativeTheme(runner, info)
}

// transientPrompt is the short prompt the accepted line is left with, or
// empty when the active theme does not offer one. Only the p10k engine
// implements it today; every other theme leaves its prompt as it was.
func transientPrompt(runner *interp.Runner, info promptInfo) string {
	if themeName(runner) != "p10k" {
		return ""
	}
	prompt, ok := promptengine.RenderTransient(p10kConfigFor(runner), p10kContext(runner, info))
	if !ok {
		return ""
	}
	return prompt
}

// themeName resolves which theme renders the next prompt. Precedence:
// a manual KOI_PROMPT (the "literal" theme) wins; colorless terminals
// force plain; otherwise KOI_THEME names a built-in or a plugin theme.
func themeName(runner *interp.Runner) string {
	if shellVar(runner, "KOI_PROMPT", "") != "" {
		return "literal"
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return "plain"
	}
	if name := shellVar(runner, "KOI_THEME", ""); name != "" {
		return name
	}
	// Nothing chosen here, but something in the session set PS1 — a
	// tool's init, or an rc the user carried over. That is a request,
	// and honoring it is what makes starship and every hand-written
	// prompt work without a koi-specific setting (#159). An explicit
	// KOI_THEME above still wins: a choice the user made in koi's own
	// vocabulary beats one inherited from somewhere else.
	if sessionPS1(runner) != "" {
		return "bash"
	}
	return "plain"
}

// nakedPrompt is the default: what a stock zsh (or bash) prompt looks
// like, so day one feels like the shell you came from.
func nakedPrompt(info promptInfo) (string, string) {
	return expandPrompt("%u@%h %W %% ", info), contPrompt
}

// themeSegment is one slot in the themed first line: an id addressable
// from KOI_THEME_SEGMENTS, its default color, and a renderer that
// returns empty when the segment has nothing to say.
type themeSegment struct {
	id     string
	color  string
	render func(info promptInfo) string
}

// themeSegments is the built-in set, in default display order. Ids not
// in this table address %p{id} plugin segments.
var themeSegments = []themeSegment{
	{"dir", cCyan, func(info promptInfo) string { return smartPath(info.dir, info.home) }},
	{"git", cMag, func(info promptInfo) string { return pluginSegment(info, "git") }},
	{"pins", cDim, func(info promptInfo) string { return toolPins(info.dir) }},
	{"jobs", cDim, func(info promptInfo) string {
		if info.jobs > 0 {
			return fmt.Sprintf("⚙%d", info.jobs)
		}
		return ""
	}},
	{"duration", cDim, func(info promptInfo) string {
		if info.duration >= 3*time.Second {
			return fmtDuration(info.duration)
		}
		return ""
	}},
	{"exit", cRed, func(info promptInfo) string {
		if info.exitCode != 0 {
			return fmt.Sprintf("✘ %d", info.exitCode)
		}
		return ""
	}},
	// time is available but not in the default left-side list — it is
	// the classic right-prompt segment.
	{"time", cDim, func(promptInfo) string { return time.Now().Format("15:04:05") }},
}

func pluginSegment(info promptInfo, id string) string {
	if info.segment == nil {
		return ""
	}
	return info.segment(id)
}

// defaultSegmentIDs is the out-of-box left-side order — deliberately an
// explicit list, not the table above: time is available but renders on
// the right by convention, and future additions must opt in here.
func defaultSegmentIDs() []string {
	return []string{"dir", "git", "pins", "jobs", "duration", "exit"}
}

// themeConfig is the resolved per-segment configuration (#28): which
// segments render, in what order, color overrides, and layout.
type themeConfig struct {
	segments  []string
	rprompt   []string          // KOI_THEME_RPROMPT: right-side segment ids
	colors    map[string]string // segment id → SGR escape
	oneLine   bool              // KOI_THEME_LINES=1: no frame, arrow inline
	noFrame   bool              // KOI_THEME_FRAME=off: two lines, no corners
	powerline bool              // KOI_THEME_SEP=powerline: chevron separators
}

func defaultThemeConfig() themeConfig {
	return themeConfig{segments: defaultSegmentIDs()}
}

// themeConfigFrom reads KOI_THEME_SEGMENTS (ordered ids; unknown ids
// address plugin segments) and KOI_THEME_COLOR_<ID> overrides. Invalid
// or empty values fall back to the defaults — a bad rc line degrades,
// it never breaks the prompt.
func themeConfigFrom(runner *interp.Runner) themeConfig {
	cfg := defaultThemeConfig()
	if v := shellVar(runner, "KOI_THEME_SEGMENTS", ""); strings.TrimSpace(v) != "" {
		cfg.segments = strings.Fields(v)
	}
	cfg.rprompt = strings.Fields(shellVar(runner, "KOI_THEME_RPROMPT", ""))
	cfg.oneLine = shellVar(runner, "KOI_THEME_LINES", "2") == "1"
	cfg.noFrame = shellVar(runner, "KOI_THEME_FRAME", "on") == "off"
	cfg.powerline = shellVar(runner, "KOI_THEME_SEP", "plain") == "powerline"
	for _, id := range slices.Concat(cfg.segments, cfg.rprompt) {
		if sgr, ok := colorSGR(shellVar(runner, themeColorVar(id), "")); ok {
			if cfg.colors == nil {
				cfg.colors = map[string]string{}
			}
			cfg.colors[id] = sgr
		}
	}
	return cfg
}

// themeColorVar maps a segment id to its color override variable:
// dir → KOI_THEME_COLOR_DIR.
func themeColorVar(id string) string {
	return "KOI_THEME_COLOR_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
}

// themeColors is the named palette accepted by KOI_THEME_COLOR_* and
// `config theme.color.<id>`; raw SGR parameter strings ("38;5;208")
// pass through untranslated.
var themeColors = map[string]string{
	"black": "30", "red": "31", "green": "32", "yellow": "33",
	"blue": "34", "magenta": "35", "cyan": "36", "white": "37",
	"bright-black": "90", "bright-red": "91", "bright-green": "92",
	"bright-yellow": "93", "bright-blue": "94", "bright-magenta": "95",
	"bright-cyan": "96", "bright-white": "97", "dim": "2",
}

var sgrRe = regexp.MustCompile(`^[0-9]+(;[0-9]+)*$`)

// colorSGR resolves a color name or raw SGR parameter string to its
// escape sequence, reporting whether the value was usable.
func colorSGR(value string) (string, bool) {
	if params, ok := themeColors[value]; ok {
		return "\x1b[" + params + "m", true
	}
	if sgrRe.MatchString(value) {
		return "\x1b[" + value + "m", true
	}
	return "", false
}

// themeSep is the between-segments separator: two spaces, or the
// nerd-font thin chevron when powerline is on.
func themeSep(cfg themeConfig) string {
	if cfg.powerline {
		return " " + cDim + "\ue0b1" + cReset + " "
	}
	return "  "
}

// themedPrompt renders the p10k-style layout — two-line by default:
//
//	╭─ ~/d/koi  main !2 ?1  go 1.26.1  ⚙1  2.3s  ✘ 7
//	╰─❯
//
// or, with KOI_THEME_LINES=1, everything inline:
//
//	~/d/koi  main !2 ?1  ✘ 7 ❯
func themedPrompt(info promptInfo, cfg themeConfig) (string, string) {
	sep := themeSep(cfg)

	var b strings.Builder
	if !cfg.oneLine && !cfg.noFrame {
		b.WriteString(cDim + "╭─ " + cReset)
	}
	if os.Getenv("SSH_CONNECTION") != "" {
		fmt.Fprintf(&b, "%s%s@%s%s ", cYel, info.username, info.host, cReset)
	}
	first := true
	for _, id := range cfg.segments {
		text, color := renderSegment(info, id)
		if text == "" {
			continue
		}
		if c, ok := cfg.colors[id]; ok {
			color = c
		}
		if !first {
			b.WriteString(sep)
		}
		first = false
		b.WriteString(color + text + cReset)
	}

	arrow := cGreen
	if info.exitCode != 0 {
		arrow = cRed
	}
	switch {
	case cfg.oneLine:
		if !first {
			b.WriteString(" ")
		}
		b.WriteString(arrow + "❯" + cReset + " ")
	case cfg.noFrame: // spaceship-style: two lines, bare arrow
		b.WriteString("\n" + arrow + "❯" + cReset + " ")
	default:
		b.WriteString("\n" + cDim + "╰─" + cReset + arrow + "❯" + cReset + " ")
	}
	return b.String(), cDim + "│ " + cReset
}

// themedRPrompt renders the right-side prompt from cfg.rprompt — same
// segment vocabulary as the left, joined with the configured separator.
// Empty when no rprompt segments are configured or none have content.
func themedRPrompt(info promptInfo, cfg themeConfig) string {
	sep := themeSep(cfg)
	var parts []string
	for _, id := range cfg.rprompt {
		text, color := renderSegment(info, id)
		if text == "" {
			continue
		}
		if c, ok := cfg.colors[id]; ok {
			color = c
		}
		parts = append(parts, color+text+cReset)
	}
	return strings.Join(parts, sep)
}

// The manual prompt surface: a free-form string with escapes, rendered
// through the theme engine like everything else (#109). Overriding the
// prompt with a literal string is table stakes, so KOI_PROMPT stays —
// but it is now the "literal" theme rather than a second pipeline.
//
// The escape set is deliberately small and deliberately *zsh-spelled*
// where zsh has a spelling, because the person writing KOI_PROMPT is
// usually porting a zsh PROMPT and their fingers know %n/%m/%~ (#96):
//
//	%n %u  username        %~ %w  cwd, ~-abbreviated
//	%m %h  hostname        %W     cwd basename
//	%d     cwd, full       %?     last exit status
//	%#     %% for a user, # for root
//	%p{id} a plugin prompt segment
//	%%     a literal %
//
// Anything else passes through untouched. That list is the contract:
// koi does not accrete zsh's full PROMPT_SUBST surface by accident.
func expandPrompt(format string, info promptInfo) string {
	var b strings.Builder
	runes := []rune(format)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '%' || i+1 >= len(runes) {
			b.WriteRune(runes[i])
			continue
		}
		i++
		switch runes[i] {
		case '%':
			b.WriteByte('%')
		case 'u', 'n': // %n is zsh's spelling
			b.WriteString(info.username)
		case 'h', 'm': // %m is zsh's spelling
			b.WriteString(info.host)
		case 'w', '~': // %~ is zsh's spelling
			b.WriteString(tildify(info.dir, info.home))
		case 'd':
			b.WriteString(info.dir)
		case '#':
			if os.Geteuid() == 0 {
				b.WriteByte('#')
			} else {
				b.WriteByte('%')
			}
		case 'W':
			if info.home != "" && info.dir == info.home {
				b.WriteByte('~')
			} else {
				b.WriteString(filepath.Base(info.dir))
			}
		case '?':
			b.WriteString(strconv.Itoa(info.exitCode))
		case 'p':
			id, next, ok := bracedArg(runes, i)
			if !ok {
				b.WriteString("%p")
				continue
			}
			i = next
			if info.segment != nil {
				b.WriteString(info.segment(id))
			}
		default:
			b.WriteByte('%')
			b.WriteRune(runes[i])
		}
	}
	return b.String()
}

// bracedArg parses {arg} starting right after the escape rune at i;
// returns the arg and the index of the closing brace.
func bracedArg(runes []rune, i int) (arg string, next int, ok bool) {
	if i+1 >= len(runes) || runes[i+1] != '{' {
		return "", i, false
	}
	for j := i + 2; j < len(runes); j++ {
		if runes[j] == '}' {
			return string(runes[i+2 : j]), j, true
		}
	}
	return "", i, false
}

// renderSegment resolves an id: built-ins from themeSegments, anything
// else a plugin segment (dim by default).
func renderSegment(info promptInfo, id string) (text, color string) {
	for _, s := range themeSegments {
		if s.id == id {
			return s.render(info), s.color
		}
	}
	return pluginSegment(info, id), cDim
}

// smartPath is the p10k-style directory: ~ abbreviation, and when the
// path runs deeper than three real components, everything but the last
// two shortens to its first rune.
func smartPath(dir, home string) string {
	path := tildify(dir, home)
	sep := string(os.PathSeparator)
	parts := strings.Split(path, sep)
	real := 0
	for _, p := range parts {
		if p != "" && p != "~" {
			real++
		}
	}
	if real <= 3 {
		return path
	}
	kept := 0 // shorten until only the last two remain full
	for i := range parts {
		if parts[i] == "" || parts[i] == "~" {
			continue
		}
		if kept < real-2 {
			if r := []rune(parts[i]); len(r) > 1 {
				parts[i] = string(r[0])
			}
		}
		kept++
	}
	return strings.Join(parts, sep)
}

// fmtDuration renders like p10k: 2.3s, 1m12s, 1h3m.
func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// toolPins reports the cwd's .tool-versions pins (asdf), cached by
// directory + mtime so prompts cost one stat, not one read.
var pinCache sync.Map // dir → pinEntry

type pinEntry struct {
	mtime time.Time
	pins  string
}

func toolPins(dir string) string {
	path := filepath.Join(dir, ".tool-versions")
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if e, ok := pinCache.Load(dir); ok {
		if entry := e.(pinEntry); entry.mtime.Equal(fi.ModTime()) {
			return entry.pins
		}
	}
	// Show the truth, not just the file (#77): a pin whose version is
	// not installed is marked. The tool builtin clears this cache on
	// pin edits and installs.
	roots := tools.InstallRoots()
	var parts []string
	for _, pin := range tools.ParseFile(path) {
		label := pin.Tool + " " + pin.Versions[0]
		if !pin.Resolves(roots) {
			label += "✗"
		}
		parts = append(parts, label)
		if len(parts) == 3 {
			break // prompt real estate: at most three pins
		}
	}
	pins := strings.Join(parts, " ")
	pinCache.Store(dir, pinEntry{mtime: fi.ModTime(), pins: pins})
	return pins
}
