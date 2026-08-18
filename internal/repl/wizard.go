package repl

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/term"
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

func newWizardIO(in io.Reader, out io.Writer) *wizardIO {
	return &wizardIO{in: bufio.NewScanner(in), out: out}
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
	w := newWizardPrompt(hc.Stdin, hc.Stdout)
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

	w.note("koi theme configurator — Enter keeps the current value, Ctrl-C aborts.")
	answers := wizardAnswers{}

	theme, ok := w.selectOne("theme", "plain is the naked default; p10k is the native two-line theme",
		get("KOI_THEME", "plain"), []string{"plain", "p10k", "starship"})
	if !ok {
		return abort()
	}
	answers["KOI_THEME"] = theme

	cfg := themeConfig{segments: strings.Fields(get("KOI_THEME_SEGMENTS", strings.Join(defaultSegmentIDs(), " ")))}
	if theme == "p10k" {
		// Ask, don't detect: whether the chevron renders is a property of
		// the user's font, p10k-style — only the human can see it.
		w.note(fmt.Sprintf("\nseparator preview   plain:  dir  main !2   powerline:  dir %s\ue0b1%s main !2",
			cDim, cReset))
		sepAnswer, sok := w.selectOne("separators", "did the powerline chevron above render?",
			get("KOI_THEME_SEP", "plain"), []string{"plain", "powerline"})
		if !sok {
			return abort()
		}
		answers["KOI_THEME_SEP"] = sepAnswer
		cfg.powerline = sepAnswer == "powerline"

		lines, lok := w.selectOne("layout", "2 = framed two-line, 1 = inline arrow",
			get("KOI_THEME_LINES", "2"), []string{"2", "1"})
		if !lok {
			return abort()
		}
		answers["KOI_THEME_LINES"] = lines
		cfg.oneLine = lines == "1"

		if lines == "2" {
			frame, fok := w.selectOne("frame", "on = ╭─/╰─ corners, off = open like spaceship",
				get("KOI_THEME_FRAME", "on"), []string{"on", "off"})
			if !fok {
				return abort()
			}
			answers["KOI_THEME_FRAME"] = frame
			cfg.noFrame = frame == "off"
		}

		list, gok := w.freeText("segments, in order",
			"built-ins: "+strings.Join(defaultSegmentIDs(), " ")+"; any %p{id} plugin id also works",
			strings.Join(cfg.segments, " "), validateSegments)
		if !gok {
			return abort()
		}
		cfg.segments = strings.Fields(list)
		answers["KOI_THEME_SEGMENTS"] = list

		preview, _ := themedPrompt(wizardSampleInfo(), cfg)
		w.note("\npreview:")
		w.note(preview)
		w.note("\nper-segment colors stay available as `config theme.color.<id> <color>`.")
	}

	save, ok := w.confirm("save?", true)
	if !ok || !save {
		return abort()
	}

	// Persist every answer that differs from the session value, then
	// hand the interpreter one eval with all the live assignments.
	var pairs [][2]string
	for _, varName := range []string{
		"KOI_THEME", "KOI_THEME_SEP", "KOI_THEME_LINES", "KOI_THEME_FRAME", "KOI_THEME_SEGMENTS",
	} {
		if value, chosen := answers[varName]; chosen && value != hc.Env.Get(varName).String() {
			pairs = append(pairs, [2]string{varName, value})
		}
	}
	if len(pairs) == 0 {
		fmt.Fprintln(hc.Stdout, "nothing changed")
		return []string{"true"}
	}
	assigns, ok := persistPairs(hc, pairs)
	if !ok {
		return []string{"false"}
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
		dir:      home + "/dev/koi",
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
