package repl

import (
	"testing"

	"github.com/blairham/gish/internal/editor"
)

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

func TestHighlightPathsAreNeverRed(t *testing.T) {
	t.Parallel()

	spans := highlightSpans("./build/gish -c x", knownSet())
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
