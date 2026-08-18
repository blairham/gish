package repl

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/editor"
)

// runnerWithVars builds a runner whose session variables are set, which
// is how a knob arrives in practice: an rc assignment or a live
// `config` change, both of which land in runner.Vars.
func runnerWithVars(t *testing.T, vars map[string]string) *interp.Runner {
	t.Helper()
	runner, err := interp.New(interp.StdIO(nil, io.Discard, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range vars {
		if v == "" {
			continue
		}
		// Assign through the interpreter rather than poking Vars: the
		// map is nil until the runner has run something, and this is the
		// path an rc line actually takes.
		if err := runner.Run(t.Context(), parseLine(t, k+"="+strconv.Quote(v))); err != nil {
			t.Fatal(err)
		}
	}
	return runner
}

func knownSet(names ...string) func(string) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(name string) bool { return set[name] }
}

func spanFor(t *testing.T, spans []editor.HighlightSpan, start int) editor.HighlightSpan {
	t.Helper()
	for _, s := range spans {
		if s.Start == start {
			return s
		}
	}
	t.Fatalf("no span starting at %d in %+v", start, spans)
	return editor.HighlightSpan{}
}

// The tests below inject a fake known-set to isolate the span logic.
// This one drives highlightFn's real predicate, because that is where
// the bug lived (#193): every span test passed for months while the
// live verdict painted aliases and koi's own commands red.
func TestHighlightFnUsesTheSessionVocabulary(t *testing.T) {
	t.Cleanup(sessionAliases.reset)
	sessionAliases.reset()
	sessionAliases.observe([]string{"alias", "ll=ls -la"})

	fn := highlightFn(runnerWithVars(t, nil))
	for word, want := range map[string]string{
		"ll":       hlGoodCmd, // alias
		"doctor":   hlGoodCmd, // CallHandler-routed command
		"builtins": hlGoodCmd, // koi-native builtin
		"cd":       hlGoodCmd, // interpreter builtin
		"zzqqxx":   hlBadCmd,  // nothing anywhere
	} {
		spans := fn(word + " arg")
		if got := spanFor(t, spans, 0).Style; got != want {
			t.Errorf("style for %q = %q, want %q", word, got, want)
		}
	}
}

func TestHighlightKnownAndUnknownCommands(t *testing.T) {
	t.Parallel()

	known := knownSet("git")
	spans := highlightSpans("git status", known)
	if s := spanFor(t, spans, 0); s.Style != hlGoodCmd || s.End != 3 {
		t.Errorf("git span = %+v", s)
	}

	spans = highlightSpans("gti status", known)
	if s := spanFor(t, spans, 0); s.Style != hlBadCmd {
		t.Errorf("typo span = %+v (want red)", s)
	}
}

// TestHighlightStatesAreBothColors pins the literals, not just the
// constants. Every other test compares spans to hlGoodCmd/hlBadCmd, so
// the whole suite passed while a resolved command carried no color at
// all — bold reads as emphasis rather than approval, leaving only the
// negative signal. Naming the escapes here is what makes that visible.
func TestHighlightStatesAreBothColors(t *testing.T) {
	t.Parallel()

	if hlGoodCmd != "\x1b[32m" {
		t.Errorf("known command style = %q, want green", hlGoodCmd)
	}
	if hlBadCmd != "\x1b[31m" {
		t.Errorf("unknown command style = %q, want red", hlBadCmd)
	}
	if hlGoodCmd == hlBadCmd {
		t.Error("the two command states are indistinguishable")
	}
}

func TestHighlightPathsAreNeverRed(t *testing.T) {
	t.Parallel()

	spans := highlightSpans("./build/koi -c x", knownSet())
	if s := spanFor(t, spans, 0); s.Style == hlBadCmd {
		t.Errorf("path command marked red: %+v", s)
	}
}

func TestHighlightStringsExpansionsComments(t *testing.T) {
	t.Parallel()

	text := `echo 'sq' "dq" $VAR $(sub) # note`
	spans := highlightSpans(text, knownSet("echo", "sub"))

	styles := map[string]bool{}
	for _, s := range spans {
		styles[s.Style] = true
	}
	for _, want := range []string{hlGoodCmd, hlString, hlExpand, hlComment} {
		if !styles[want] {
			t.Errorf("missing style %q in %+v", want, spans)
		}
	}
}

func TestHighlightFallbackOnParseError(t *testing.T) {
	t.Parallel()

	// Unclosed quote: mid-edit state. The first-word check survives.
	spans := highlightSpans(`gti "unclosed`, knownSet("git"))
	if len(spans) != 1 || spans[0].Style != hlBadCmd {
		t.Errorf("fallback spans = %+v", spans)
	}

	spans = highlightSpans("   ", knownSet())
	if len(spans) != 0 {
		t.Errorf("whitespace-only spans = %+v", spans)
	}
}

func TestHighlightPipelineCommands(t *testing.T) {
	t.Parallel()

	spans := highlightSpans("git log | gerp x", knownSet("git"))
	if s := spanFor(t, spans, 0); s.Style != hlGoodCmd {
		t.Errorf("first cmd = %+v", s)
	}
	if s := spanFor(t, spans, 10); s.Style != hlBadCmd {
		t.Errorf("second cmd = %+v (want red)", s)
	}
}

// The per-feature knob (#163). Someone left a shell over the red flash
// on a half-typed command; NO_COLOR is not an answer to that, because
// it takes the whole prompt's color with it.
func TestHighlightModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		set        string
		wantMode   string
		wantVerdct bool // is the command word colored?
	}{
		{"", highlightOn, true},
		{"on", highlightOn, true},
		{"quiet", highlightQuiet, false},
		{"QUIET", highlightQuiet, false}, // case is not a configuration error
		{"off", highlightOff, false},
		{"nonsense", highlightOn, true}, // a typo in an rc never breaks the prompt
	}
	for _, tt := range tests {
		t.Run("KOI_HIGHLIGHT="+tt.set, func(t *testing.T) {
			runner := runnerWithVars(t, map[string]string{"KOI_HIGHLIGHT": tt.set})
			if got := highlightMode(runner); got != tt.wantMode {
				t.Errorf("highlightMode = %q, want %q", got, tt.wantMode)
			}
			spans := highlightFn(runner)(`nosuchcmd "arg"`)
			if tt.wantMode == highlightOff && spans != nil {
				t.Errorf("off still highlighted: %+v", spans)
			}
			verdict := false
			for _, s := range spans {
				if s.Start == 0 && (s.Style == hlBadCmd || s.Style == hlGoodCmd) {
					verdict = true
				}
			}
			if verdict != tt.wantVerdct {
				t.Errorf("command verdict = %v, want %v (spans %+v)", verdict, tt.wantVerdct, spans)
			}
			// quiet keeps the rest: the complaint was the verdict, not color.
			if tt.wantMode == highlightQuiet && len(spans) == 0 {
				t.Error("quiet dropped every span, not just the verdict")
			}
		})
	}
}

// quiet mode must also stay quiet on the mid-edit path, which is where
// the flash actually happens — the buffer does not parse for most of the
// keystrokes that build a command.
func TestHighlightQuietSurvivesUnparsableBuffers(t *testing.T) {
	t.Parallel()

	if spans := highlightSpans(`nosuchcmd "unclosed`, nil); spans != nil {
		t.Errorf("quiet fallback produced spans: %+v", spans)
	}
	if spans := highlightSpans(`nosuchcmd "unclosed`, knownSet()); len(spans) == 0 {
		t.Error("the normal fallback stopped coloring the command word")
	}
}

// Ghost text gets its own switch for the same reason (#163).
func TestSuggestKnob(t *testing.T) {
	t.Parallel()

	for set, want := range map[string]bool{"": true, "on": true, "off": false, "OFF": false} {
		if got := suggestEnabled(runnerWithVars(t, map[string]string{"KOI_SUGGEST": set})); got != want {
			t.Errorf("KOI_SUGGEST=%q enabled = %v, want %v", set, got, want)
		}
	}
}

// Do not impose a palette (#163): the built-in styles must be the
// terminal's own 16 colors, which the user has already themed, and never
// 256-color or truecolor SGR — those are the codes that make a shell
// look wrong in someone's carefully matched scheme.
func TestBuiltInStylesUseTheTerminalPalette(t *testing.T) {
	t.Parallel()

	for _, style := range []string{hlBadCmd, hlGoodCmd, hlString, hlExpand, hlComment} {
		body := strings.TrimSuffix(strings.TrimPrefix(style, "\x1b["), "m")
		for _, param := range strings.Split(body, ";") {
			n, err := strconv.Atoi(param)
			if err != nil {
				t.Fatalf("style %q has a non-numeric SGR param %q", style, param)
			}
			switch {
			case n == 38 || n == 48:
				t.Errorf("style %q selects an extended color; use the terminal's palette", style)
			case n >= 0 && n <= 9, n >= 30 && n <= 37, n >= 40 && n <= 47:
				// attributes and the base 16: fine — the user themed these
			default:
				t.Errorf("style %q uses SGR %d, outside the base palette", style, n)
			}
		}
	}
}
