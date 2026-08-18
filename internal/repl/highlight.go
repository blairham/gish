package repl

import (
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/editor"
	"github.com/blairham/koi-shell/internal/history"
)

// Highlight styles (#38). Kept minimal and legible: the signature
// affordance is red-for-unknown-command — fish's "see the typo before
// Enter".
//
// A resolved command is green rather than merely bold, because red and
// bold are not opposites: bold reads as emphasis, not as approval, so
// the only real signal was the negative one. Green is also what
// zsh-syntax-highlighting uses for commands, builtins, functions and
// aliases, and that is the muscle memory arrivals bring with them.
// Both states now say something, which is the point of coloring the
// word at all.
const (
	hlBadCmd  = "\x1b[31m" // unknown command: red
	hlGoodCmd = "\x1b[32m" // known command: green
	hlString  = "\x1b[33m" // quoted strings: yellow
	hlExpand  = "\x1b[36m" // $VAR, ${…}, $(…): cyan
	hlComment = "\x1b[2m"  // comments: dim
)

// Highlight modes (#163). NO_COLOR is all-or-nothing, and the
// documented churn case is narrower than that: someone left a shell
// over the red-on-unknown-command flash while typing, not over syntax
// color as such. So the knob is per-feature —
//
//	on     everything (default)
//	quiet  syntax color, but no verdict on the command word
//	off    no highlighting at all
//
// `quiet` exists because the unknown-command color is the one span that
// is *wrong* half the time it appears: a command is unknown until its
// last letter is typed, so the signal fires on every correct command on
// the way to being correct.
const (
	highlightOn    = "on"
	highlightQuiet = "quiet"
	highlightOff   = "off"
)

// highlightMode reads the live setting, so an rc that sets it — or a
// mid-session `config highlight quiet` — takes effect without rebuilding
// the editor. Unknown values mean the default: a typo in an rc must
// never be why the shell looks broken.
func highlightMode(runner *interp.Runner) string {
	switch strings.ToLower(shellVar(runner, "KOI_HIGHLIGHT", highlightOn)) {
	case highlightOff:
		return highlightOff
	case highlightQuiet:
		return highlightQuiet
	default:
		return highlightOn
	}
}

// suggestEnabled reports whether history ghost text is wanted. Same
// reasoning as the highlight knob: the autosuggestion is fish's most
// divisive affordance, and "turn the whole shell monochrome" is not an
// acceptable way to switch one feature off.
func suggestEnabled(runner *interp.Runner) bool {
	return !strings.EqualFold(shellVar(runner, "KOI_SUGGEST", "on"), "off")
}

// suggestFn builds the editor's ghost-text hook: the newest history
// entry extending what is typed, or nothing when the knob is off.
//
// It is a named function rather than a closure at the wiring site so a
// test can drive the *real* pair — the live knob and the real store —
// the way highlightFn is driven. An anonymous closure there is
// unreachable from any test, which is exactly how the highlighter's own
// wiring stayed broken for months (#193).
func suggestFn(runner *interp.Runner, store *history.Store) func(string) string {
	return func(text string) string {
		if !suggestEnabled(runner) {
			return ""
		}
		if s, ok := store.Match(text, 0); ok {
			return s
		}
		return ""
	}
}

// highlightFn builds the editor's highlighter: spans derived from a real
// parse of the buffer (we hold the actual bash parser — no regexes).
// This runs per keystroke, so everything here is local and cheap: the
// command-existence check hits the completion machinery's cached PATH
// set, and the mode lookup is a map read.
func highlightFn(runner *interp.Runner) func(string) []editor.HighlightSpan {
	return func(text string) []editor.HighlightSpan {
		mode := highlightMode(runner)
		if mode == highlightOff {
			return nil
		}
		// The verdict draws on the shared session vocabulary (#193):
		// aliases, koi's CallHandler-routed commands, native builtins,
		// functions, plugin commands, and PATH. A private list here is
		// how every alias and most of koi's own commands spent months
		// rendering red — valid, and painted as typos.
		known := func(name string) bool { return knownCommand(runner, name) }
		if mode == highlightQuiet {
			known = nil // no verdict on the command word
		}
		return highlightSpans(text, known)
	}
}

// highlightSpans parses text and colors it from the syntax tree. When
// the parse fails (the line is mid-edit most of the time), it falls back
// to first-word command coloring so the signature affordance survives.
//
// A nil known is quiet mode: the command word gets no verdict, and
// everything else is styled as usual.
func highlightSpans(text string, known func(string) bool) []editor.HighlightSpan {
	parser := syntax.NewParser(syntax.KeepComments(true))
	file, err := parser.Parse(strings.NewReader(text), "")
	if err != nil {
		return fallbackSpans(text, known)
	}

	runeAt := byteToRune(text)
	var spans []editor.HighlightSpan
	add := func(start, end syntax.Pos, style string) {
		s, e := runeAt(int(start.Offset())), runeAt(int(end.Offset()))
		if s < e {
			spans = append(spans, editor.HighlightSpan{Start: s, End: e, Style: style})
		}
	}

	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CallExpr:
			if len(n.Args) > 0 && known != nil {
				if lit := flatLiteral(n.Args[0]); lit != "" {
					style := hlGoodCmd
					if !known(lit) && !strings.Contains(lit, "/") {
						style = hlBadCmd
					}
					add(n.Args[0].Pos(), n.Args[0].End(), style)
				}
			}
		case *syntax.SglQuoted:
			add(n.Pos(), n.End(), hlString)
		case *syntax.DblQuoted:
			add(n.Pos(), n.End(), hlString)
		case *syntax.ParamExp:
			add(n.Pos(), n.End(), hlExpand)
		case *syntax.CmdSubst:
			add(n.Pos(), n.End(), hlExpand)
		case *syntax.Comment:
			add(n.Pos(), n.End(), hlComment)
		}
		return true
	})

	return normalizeSpans(spans)
}

// flatLiteral returns a word's value when it is a plain literal (the
// only case where command-existence coloring is meaningful).
func flatLiteral(w *syntax.Word) string {
	if len(w.Parts) != 1 {
		return ""
	}
	lit, ok := w.Parts[0].(*syntax.Lit)
	if !ok {
		return ""
	}
	return lit.Value
}

// fallbackSpans colors just the first word on a line that doesn't parse
// yet (unclosed quote, trailing pipe): the typo check keeps working
// mid-edit.
func fallbackSpans(text string, known func(string) bool) []editor.HighlightSpan {
	if known == nil {
		return nil // quiet mode: the fallback is only ever the verdict
	}
	runes := []rune(text)
	start := 0
	for start < len(runes) && (runes[start] == ' ' || runes[start] == '\t') {
		start++
	}
	end := start
	for end < len(runes) && runes[end] != ' ' && runes[end] != '\t' && runes[end] != '\n' {
		end++
	}
	if start == end {
		return nil
	}
	word := string(runes[start:end])
	style := hlGoodCmd
	if !known(word) && !strings.Contains(word, "/") {
		style = hlBadCmd
	}
	return []editor.HighlightSpan{{Start: start, End: end, Style: style}}
}

// byteToRune maps byte offsets (the parser's coordinates) to rune
// offsets (the editor's).
func byteToRune(text string) func(int) int {
	return func(byteOff int) int {
		if byteOff > len(text) {
			byteOff = len(text)
		}
		count := 0
		for i := range text {
			if i >= byteOff {
				return count
			}
			count++
		}
		return count
	}
}

// normalizeSpans sorts spans and drops overlaps: outer nodes were added
// first by the walk, but the editor applies spans linearly, so nested
// spans (a $VAR inside a "string") win by coming first at their start.
func normalizeSpans(spans []editor.HighlightSpan) []editor.HighlightSpan {
	// Inner nodes are visited after outer ones; prefer the innermost by
	// splitting outer spans around inner ones. v1 keeps it simple:
	// sort by start, and on ties prefer the shorter (inner) span; the
	// applier skips overlapped remainders.
	out := make([]editor.HighlightSpan, len(spans))
	copy(out, spans)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if b.Start < a.Start || (b.Start == a.Start && b.End-b.Start < a.End-a.Start) {
				out[j-1], out[j] = b, a
			} else {
				break
			}
		}
	}
	return out
}

// editModeOf resolves the line editor's dialect (#163).
//
// vi mode is a documented abandonment cause in both directions: its
// absence drove people back to zsh, and NO_COLOR-style all-or-nothing
// switches are not how anyone expects to reach it. KOI_EDIT_MODE is
// the knob, `config editmode vi` sets it, and `set -o vi` in an rc is
// honored too — that line is in a great many inherited rc files, and
// silently ignoring it would be the same class of trap the `alias`
// no-op was.
func editModeOf(runner *interp.Runner) editor.EditMode {
	if strings.EqualFold(shellVar(runner, "KOI_EDIT_MODE", "emacs"), "vi") {
		return editor.ModeVi
	}
	return editor.ModeEmacs
}
