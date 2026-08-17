package repl

import (
	"testing"
)

// Fuzz the highlighter over arbitrary buffers. It parses with the real
// bash parser on every keystroke, which means its steady-state input is
// exactly the malformed: half-typed commands, unclosed quotes, pasted
// fragments. The editor applies the spans it returns with no further
// validation, so the bounds and ordering asserted here are load-bearing.
func FuzzHighlightSpans(f *testing.F) {
	for _, seed := range []string{
		"ls -la", "git commit -m 'wip", `echo "unclosed`, "a | b | c",
		"x=1 y=2 cmd", "$(nested $(deeper))", "# just a comment",
		"cat <<EOF\nline\nEOF", "for i in *; do echo $i; done",
		"cmd 'sq' \"dq\" $VAR ${VAR:-def} $(sub) `bt` # tail",
		"héllo wörld 🐚", "\x00\x01\x02", "((", "))", "&&", ";;",
		"very/long/path/cmd --flag=value", "!", "\\", "\n\n\n",
	} {
		f.Add(seed, true)
	}

	f.Fuzz(func(t *testing.T, text string, known bool) {
		spans := highlightSpans(text, func(string) bool { return known })

		runeLen := len([]rune(text))
		styles := map[string]bool{
			hlGoodCmd: true, hlBadCmd: true, hlString: true,
			hlExpand: true, hlComment: true,
		}
		prevStart := -1
		for i, s := range spans {
			// The editor slices the rune buffer with these offsets; an
			// out-of-range or inverted span is a render-time panic.
			if s.Start < 0 || s.End > runeLen || s.Start >= s.End {
				t.Fatalf("span %d out of bounds: %+v for %q (len %d)", i, s, text, runeLen)
			}
			// normalizeSpans promises start order — the applier walks
			// linearly and silently drops anything that steps backward.
			if s.Start < prevStart {
				t.Fatalf("span %d unsorted: %+v after start %d in %q", i, s, prevStart, text)
			}
			prevStart = s.Start
			if !styles[s.Style] {
				t.Fatalf("span %d carries unknown style %+v for %q", i, s, text)
			}
		}

		// Quiet mode (nil known) must never emit a command verdict, on
		// any buffer — parsable or not.
		for _, s := range highlightSpans(text, nil) {
			if s.Style == hlGoodCmd || s.Style == hlBadCmd {
				t.Fatalf("quiet mode emitted a verdict span %+v for %q", s, text)
			}
		}
	})
}
