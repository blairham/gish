package repl

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// Themes. gish starts naked: the out-of-box prompt is the familiar
// zsh/bash shape (user@host dir %) with no color and no branding —
// switching shells shouldn't change how the terminal greets you until
// you ask it to. The p10k-class two-line prompt (#26, built natively on
// the segment engine — async plugin segments, budget deadlines,
// stale-serve) and starship (#45) are explicit opt-ins.
//
// Precedence: a user-set GISH_PROMPT always wins (the bare-metal escape
// hatch); otherwise GISH_THEME selects "p10k", "starship", or the naked
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

// promptStrings resolves the prompt pair for the next read. Precedence:
// manual GISH_PROMPT > GISH_THEME (starship | p10k) > the naked default;
// NO_COLOR and dumb terminals degrade to naked regardless.
func promptStrings(runner *interp.Runner, info promptInfo) (string, string) {
	if v := shellVar(runner, "GISH_PROMPT", ""); v != "" {
		return expandPrompt(v, info),
			expandPrompt(shellVar(runner, "GISH_PROMPT_CONT", contPrompt), info)
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return nakedPrompt(info)
	}
	switch shellVar(runner, "GISH_THEME", "plain") {
	case "starship":
		if p, cp, ok := starship.render(info, info.width); ok {
			return p, cp
		}
		return themedPrompt(info, themeConfigFrom(runner)) // missing binary: native fallback
	case "p10k":
		return themedPrompt(info, themeConfigFrom(runner))
	default:
		return nakedPrompt(info)
	}
}

// nakedPrompt is the default: what a stock zsh (or bash) prompt looks
// like, so day one feels like the shell you came from.
func nakedPrompt(info promptInfo) (string, string) {
	return expandPrompt("%u@%h %W %% ", info), contPrompt
}

// themeSegment is one slot in the themed first line: an id addressable
// from GISH_THEME_SEGMENTS, its default color, and a renderer that
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
	rprompt   []string          // GISH_THEME_RPROMPT: right-side segment ids
	colors    map[string]string // segment id → SGR escape
	oneLine   bool              // GISH_THEME_LINES=1: no frame, arrow inline
	noFrame   bool              // GISH_THEME_FRAME=off: two lines, no corners
	powerline bool              // GISH_THEME_SEP=powerline: chevron separators
}

func defaultThemeConfig() themeConfig {
	return themeConfig{segments: defaultSegmentIDs()}
}

// themeConfigFrom reads GISH_THEME_SEGMENTS (ordered ids; unknown ids
// address plugin segments) and GISH_THEME_COLOR_<ID> overrides. Invalid
// or empty values fall back to the defaults — a bad rc line degrades,
// it never breaks the prompt.
func themeConfigFrom(runner *interp.Runner) themeConfig {
	cfg := defaultThemeConfig()
	if v := shellVar(runner, "GISH_THEME_SEGMENTS", ""); strings.TrimSpace(v) != "" {
		cfg.segments = strings.Fields(v)
	}
	cfg.rprompt = strings.Fields(shellVar(runner, "GISH_THEME_RPROMPT", ""))
	cfg.oneLine = shellVar(runner, "GISH_THEME_LINES", "2") == "1"
	cfg.noFrame = shellVar(runner, "GISH_THEME_FRAME", "on") == "off"
	cfg.powerline = shellVar(runner, "GISH_THEME_SEP", "plain") == "powerline"
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
// dir → GISH_THEME_COLOR_DIR.
func themeColorVar(id string) string {
	return "GISH_THEME_COLOR_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
}

// themeColors is the named palette accepted by GISH_THEME_COLOR_* and
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
//	╭─ ~/d/gish  main !2 ?1  go 1.26.1  ⚙1  2.3s  ✘ 7
//	╰─❯
//
// or, with GISH_THEME_LINES=1, everything inline:
//
//	~/d/gish  main !2 ?1  ✘ 7 ❯
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

// rpromptString resolves the right prompt for the next read. It follows
// promptStrings' precedence but only the native theme produces one: a
// manual GISH_PROMPT, plain, starship, NO_COLOR, and dumb terminals all
// mean no right prompt.
func rpromptString(runner *interp.Runner, info promptInfo) string {
	if shellVar(runner, "GISH_PROMPT", "") != "" {
		return ""
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return ""
	}
	if shellVar(runner, "GISH_THEME", "plain") != "p10k" {
		return ""
	}
	return themedRPrompt(info, themeConfigFrom(runner))
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
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var parts []string
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) >= 2 && !strings.HasPrefix(fields[0], "#") {
			parts = append(parts, fields[0]+" "+fields[1])
		}
		if len(parts) == 3 {
			break // prompt real estate: at most three pins
		}
	}
	pins := strings.Join(parts, " ")
	pinCache.Store(dir, pinEntry{mtime: fi.ModTime(), pins: pins})
	return pins
}
