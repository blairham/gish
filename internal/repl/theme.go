package repl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// The default theme (#26): a powerlevel10k-class two-line prompt built
// natively on the segment engine — async plugin segments, budget
// deadlines, stale-serve — rather than by running any theme's zsh code.
//
// Precedence: a user-set GISH_PROMPT always wins (the bare-metal escape
// hatch); otherwise GISH_THEME selects "p10k" (default) or "plain".
// Dumb terminals and NO_COLOR fall back to plain automatically.

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
// manual GISH_PROMPT > GISH_THEME (starship | p10k default | plain) >
// plain; NO_COLOR and dumb terminals degrade regardless.
func promptStrings(runner *interp.Runner, info promptInfo) (string, string) {
	if v := shellVar(runner, "GISH_PROMPT", ""); v != "" {
		return expandPrompt(v, info),
			expandPrompt(shellVar(runner, "GISH_PROMPT_CONT", contPrompt), info)
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return prompt, contPrompt
	}
	switch shellVar(runner, "GISH_THEME", "p10k") {
	case "starship":
		if p, cp, ok := starship.render(info, info.width); ok {
			return p, cp
		}
		return themedPrompt(info) // missing binary: native fallback
	case "p10k":
		return themedPrompt(info)
	default:
		return prompt, contPrompt
	}
}

// themedPrompt renders the p10k-style layout:
//
//	╭─ ~/d/gish  main !2 ?1  go 1.26.1  ⚙1  2.3s  ✘ 7
//	╰─❯
func themedPrompt(info promptInfo) (string, string) {
	var b strings.Builder
	b.WriteString(cDim + "╭─ " + cReset)

	if os.Getenv("SSH_CONNECTION") != "" {
		fmt.Fprintf(&b, "%s%s@%s%s ", cYel, info.username, info.host, cReset)
	}
	b.WriteString(cCyan + smartPath(info.dir, info.home) + cReset)
	if info.segment != nil {
		if git := info.segment("git"); git != "" {
			b.WriteString("  " + cMag + git + cReset)
		}
	}
	if pins := toolPins(info.dir); pins != "" {
		b.WriteString("  " + cDim + pins + cReset)
	}
	if info.jobs > 0 {
		fmt.Fprintf(&b, "  %s⚙%d%s", cDim, info.jobs, cReset)
	}
	if info.duration >= 3*time.Second {
		fmt.Fprintf(&b, "  %s%s%s", cDim, fmtDuration(info.duration), cReset)
	}
	if info.exitCode != 0 {
		fmt.Fprintf(&b, "  %s✘ %d%s", cRed, info.exitCode, cReset)
	}
	b.WriteString("\n")

	arrow := cGreen
	if info.exitCode != 0 {
		arrow = cRed
	}
	b.WriteString(cDim + "╰─" + cReset + arrow + "❯" + cReset + " ")
	return b.String(), cDim + "│ " + cReset
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
