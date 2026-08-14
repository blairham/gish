package repl

import (
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/complete"
	"github.com/blairham/gish/internal/editor"
)

// Highlight styles (#38). Kept minimal and legible: the signature
// affordance is red-for-unknown-command — fish's "see the typo before
// Enter".
const (
	hlBadCmd  = "\x1b[31m" // unknown command: red
	hlGoodCmd = "\x1b[1m"  // known command: bold
	hlString  = "\x1b[33m" // quoted strings: yellow
	hlExpand  = "\x1b[36m" // $VAR, ${…}, $(…): cyan
	hlComment = "\x1b[2m"  // comments: dim
)

// highlightFn builds the editor's highlighter: spans derived from a real
// parse of the buffer (we hold the actual bash parser — no regexes).
// This runs per keystroke, so everything here is local and cheap: the
// command-existence check hits the completion machinery's cached PATH
// set.
func highlightFn(runner *interp.Runner) func(string) []editor.HighlightSpan {
	return func(text string) []editor.HighlightSpan {
		known := func(name string) bool {
			if interp.IsBuiltin(name) || name == "zi" {
				return true
			}
			if _, ok := runner.Funcs[name]; ok {
				return true
			}
			return complete.IsCommand(name, pathVar(runner))
		}
		return highlightSpans(text, known)
	}
}

// highlightSpans parses text and colors it from the syntax tree. When
// the parse fails (the line is mid-edit most of the time), it falls back
// to first-word command coloring so the signature affordance survives.
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
			if len(n.Args) > 0 {
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
