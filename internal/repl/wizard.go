package repl

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/term"
)

// The theme wizard (#28): `config theme` on a terminal walks through
// the knobs p10k-configure-style — ask, preview, confirm — and writes
// the answers through the same persist-and-go-live path as every other
// config set. Enter keeps the current value everywhere, and nothing is
// saved until the final confirmation. Piped or test stdin falls back
// to the plain one-line show.

// stdinIsTTY reports whether the handler's stdin is an interactive
// terminal — the gate between the wizard and the plain show.
func stdinIsTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(f)
}

// wizardIO wraps the Q&A: one scanner for the session, prompts echoing
// the default, EOF (Ctrl-D) reported so the wizard can abort cleanly.
type wizardIO struct {
	in  *bufio.Scanner
	out io.Writer
}

// ask prints "prompt (def): " and returns the trimmed answer, the
// default when the answer is empty, and ok=false on EOF.
func (w *wizardIO) ask(prompt, def string) (string, bool) {
	fmt.Fprintf(w.out, "%s (%s): ", prompt, def)
	if !w.in.Scan() {
		fmt.Fprintln(w.out)
		return "", false
	}
	answer := strings.TrimSpace(w.in.Text())
	if answer == "" {
		return def, true
	}
	return answer, true
}

// askOneOf re-asks until the answer is in the allowed set (or EOF).
func (w *wizardIO) askOneOf(prompt, def string, allowed []string) (string, bool) {
	for {
		answer, ok := w.ask(prompt, def)
		if !ok {
			return "", false
		}
		if slices.Contains(allowed, answer) {
			return answer, true
		}
		fmt.Fprintf(w.out, "  pick one of: %s\n", strings.Join(allowed, " | "))
	}
}

// wizardAnswers is what the walkthrough collects before anything is
// written: variable name → chosen value.
type wizardAnswers map[string]string

// runThemeWizard drives the walkthrough against the session state and,
// on confirmation, persists every changed variable and returns the live
// assignments. Abort (Ctrl-D or answering n) saves nothing.
func runThemeWizard(hc interp.HandlerContext) []string {
	w := &wizardIO{in: bufio.NewScanner(hc.Stdin), out: hc.Stdout}
	get := func(varName, def string) string {
		if v := hc.Env.Get(varName).String(); v != "" {
			return v
		}
		return def
	}
	abort := func() []string {
		fmt.Fprintln(hc.Stdout, "theme wizard: nothing saved")
		return []string{"true"}
	}

	fmt.Fprintln(hc.Stdout, "gish theme configurator — Enter keeps the current value, Ctrl-D aborts.")
	answers := wizardAnswers{}

	theme, ok := w.askOneOf("theme [plain/p10k/starship]", get("GISH_THEME", "plain"),
		[]string{"plain", "p10k", "starship"})
	if !ok {
		return abort()
	}
	answers["GISH_THEME"] = theme

	cfg := themeConfig{segments: strings.Fields(get("GISH_THEME_SEGMENTS", strings.Join(defaultSegmentIDs(), " ")))}
	if theme == "p10k" {
		// Separators: show the glyph and ask, p10k-style — rendering is a
		// font property only the human can judge.
		fmt.Fprintf(hc.Stdout, "\nseparator preview   plain:  dir  main !2   powerline:  dir %s\ue0b1%s main !2\n",
			cDim, cReset)
		sepAnswer, sok := w.askOneOf("separators — did the powerline chevron render? [plain/powerline]",
			get("GISH_THEME_SEP", "plain"), []string{"plain", "powerline"})
		if !sok {
			return abort()
		}
		answers["GISH_THEME_SEP"] = sepAnswer
		cfg.powerline = sepAnswer == "powerline"

		lines, lok := w.askOneOf("layout — framed two-line or single line? [2/1]",
			get("GISH_THEME_LINES", "2"), []string{"1", "2"})
		if !lok {
			return abort()
		}
		answers["GISH_THEME_LINES"] = lines
		cfg.oneLine = lines == "1"

		fmt.Fprintf(hc.Stdout, "\nsegments — built-ins: %s; any %%p{id} plugin id also works\n",
			strings.Join(defaultSegmentIDs(), " "))
		for {
			list, gok := w.ask("segments, in order", strings.Join(cfg.segments, " "))
			if !gok {
				return abort()
			}
			ids := strings.Fields(list)
			valid := len(ids) > 0
			for _, id := range ids {
				if !segmentIDRe.MatchString(id) {
					fmt.Fprintf(hc.Stdout, "  bad segment id %q\n", id)
					valid = false
				}
			}
			if valid {
				cfg.segments = ids
				answers["GISH_THEME_SEGMENTS"] = list
				break
			}
		}

		fmt.Fprintln(hc.Stdout, "\npreview:")
		preview, _ := themedPrompt(wizardSampleInfo(), cfg)
		fmt.Fprintln(hc.Stdout, preview)
		fmt.Fprintln(hc.Stdout, "\nper-segment colors stay available as `config theme.color.<id> <color>`.")
	}

	confirm, ok := w.askOneOf("save? [y/n]", "y", []string{"y", "n"})
	if !ok || confirm == "n" {
		return abort()
	}

	// Persist every answer that differs from the session value, then
	// hand the interpreter one eval with all the live assignments.
	var assigns []string
	for _, varName := range []string{"GISH_THEME", "GISH_THEME_SEP", "GISH_THEME_LINES", "GISH_THEME_SEGMENTS"} {
		value, chosen := answers[varName]
		if !chosen || value == hc.Env.Get(varName).String() {
			continue
		}
		quoted, err := syntax.Quote(value, syntax.LangBash)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "config:", err)
			return []string{"false"}
		}
		if _, err := writeRCSetting(varName, quoted); err != nil {
			fmt.Fprintln(hc.Stderr, "config:", err)
			return []string{"false"}
		}
		assigns = append(assigns, varName+"="+quoted)
	}
	if len(assigns) == 0 {
		fmt.Fprintln(hc.Stdout, "nothing changed")
		return []string{"true"}
	}
	if path, err := rcWritePath(); err == nil {
		fmt.Fprintf(hc.Stdout, "saved to %s\n", displayPath(path))
	}
	return append([]string{"eval"}, strings.Join(assigns, " "))
}

// wizardSampleInfo is the canned prompt state the preview renders —
// realistic enough to judge the layout without touching real state.
func wizardSampleInfo() promptInfo {
	home, _ := os.UserHomeDir()
	info := promptInfo{
		username: "you",
		host:     "host",
		home:     home,
		dir:      home + "/dev/gish",
		duration: 4 * time.Second,
		segment: func(id string) string {
			if id == "git" {
				return "main !2"
			}
			return ""
		},
	}
	return info
}
