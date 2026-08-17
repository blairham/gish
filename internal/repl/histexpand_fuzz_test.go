package repl

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Fuzz history expansion: it runs on every raw interactive line before
// parsing, so its input is arbitrary — half-typed commands, pasted
// binaries, hostile bytes. The properties below are the contract the
// REPL relies on; a panic here is a shell that dies on a keystroke.
func FuzzExpandHistory(f *testing.F) {
	for _, seed := range []string{
		"!!", "!$", "!^", "!:0", "!:2", "!:1-3", "!:10-2", "!git",
		"echo !!", "echo !$ done", "sudo !!", "!! && !!",
		"^old^new", "^^", "^old^new^extra^more", "^missing^x",
		"'quoted !!'", `"double !$"`, `\!escaped`, "mix '!' \"!$\" !!",
		"!", "! ", "!=", "!(", "!\t", "trailing !",
		"héllo !! wörld", "emoji 🐚 !$", "!:999999999999999999",
		"!:1-", "!:-2", "!:", "^", "^a", "!-", "!.", "!/bin/ls",
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}

	f.Fuzz(func(t *testing.T, line string, found bool) {
		last := func(prefix string, _ int) (string, bool) {
			if !found {
				return "", false
			}
			return "prev-cmd --flag arg1 arg2 " + prefix, true
		}

		out, changed, err := expandHistory(line, last)

		switch {
		case err != nil:
			// An aborted line carries no partial expansion the loop
			// could accidentally run.
			if out != "" {
				t.Fatalf("error %v but output %q", err, out)
			}
		case !changed:
			// Unchanged must mean untouched — a rewrite the flag does
			// not report would skip the bash-style echo of what runs.
			if out != line {
				t.Fatalf("changed=false but %q became %q", line, out)
			}
		default:
			// The expansion pipeline is rune-based; it must never
			// manufacture invalid UTF-8 from valid input.
			if utf8.ValidString(line) && !utf8.ValidString(out) {
				t.Fatalf("valid input %q expanded to invalid UTF-8 %q", line, out)
			}
		}

		// A line with no designators must pass through untouched.
		if !strings.ContainsAny(line, "!^") && (changed || err != nil) {
			t.Fatalf("no-designator line %q reported changed=%v err=%v", line, changed, err)
		}
	})
}
