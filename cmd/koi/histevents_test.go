//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The event designators (#692), $histchars (#695), the lines a statement
// hook cannot see (#693) and the standard-input reading path (#694).
//
// All differential, and all over a real *file* for the reason #559's
// cases are: the unit is the physical line, so an option or a variable
// set on one line reaches the next and never its own, which a `-c` string
// cannot show. The one exception is the standard-input case, which is
// about a reading path a file never takes.

// TestHistoryEventDesignators covers the forms that name an entry by
// number or by search — `!n`, `!-n`, `!?string?` — and the word
// designators bash lets follow an event with no `:` in front of them.
func TestHistoryEventDesignators(t *testing.T) {
	if testing.Short() {
		t.Skip("differential history expansion skipped in -short")
	}
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	for _, tc := range []struct{ name, script, oracle string }{
		{
			// `!n` is the entry the `history` listing prints as n, which
			// is what makes the two agree by construction rather than by
			// coincidence — the listing is in the script so a shell that
			// numbered differently fails on the numbers as well.
			name: "an event by number",
			script: `set -o history -H
echo alpha
echo beta
echo gamma
history
!1
!3
!0
!99
echo end
`,
			oracle: "event not found",
		},
		{
			// `!-n` counts back from the line being read, `!-1` being the
			// previous command — which is the entry above this line and
			// not this one.
			name: "an event counted back",
			script: `set -o history -H
echo alpha
echo beta
!-1
!-3
!-99
echo end
`,
			oracle: "echo beta",
		},
		{
			// `!?string?` is the one form that searches rather than
			// anchors, and its closing character is optional at the end
			// of a line.
			name: "an event by substring",
			script: `set -o history -H
echo one two three
echo four
!?two?
echo A !?hree?more
!?two
!?nosuchtext?
echo end
`,
			oracle: "echo one two three",
		},
		{
			// Word designators and modifiers chain off these exactly as
			// they do off `!!`, so the event half is the only new thing —
			// asserted rather than assumed.
			name: "designators chain off every event form",
			script: `set -o history -H
echo one two three four
echo M
echo A !1:2
echo B !-1:1
echo C !?two?:3-4
echo D !1:0:t
echo E !?three?:%
echo end
`,
			oracle: "echo A two",
		},
		{
			// A word designator may follow an event with no `:`, and
			// bash's set for that is the characters that end an event's
			// own text — which is why `!shopt-1` is words 0-1 of the
			// `shopt` line rather than a search for `shopt-1`.
			name: "a word designator with no colon",
			script: `set -o history -H
true one two three
echo M
echo A !true-1
echo B !true*
echo C !true^
echo D !true$
echo E !!*
echo F !-1$
echo end
`,
			oracle: "echo A true one",
		},
		{
			// A range may leave its start out — `-$` is `0-$` — and may
			// end at `$`, neither of which koi could read.
			name: "ranges with an absent start and a dollar end",
			script: `set -o history -H
echo one two three four
echo A !!:-$
echo B !-1:1-$
echo C !-2:2-
echo D !!:-
echo end
`,
			oracle: "echo A echo one two three four",
		},
		{
			// The wording of a refusal, which is what a reader acts on:
			// bash has three different messages here and koi had one.
			name: "the three refusals have three messages",
			script: `set -o history -H
echo one two
echo M
echo !!:9
echo !!:1-9
echo !!:z
echo !!:
echo !nosuchprefix
echo end
`,
			oracle: "bad word specifier",
		},
		{
			// `history -p` has one wording for every failure and names
			// the argument rather than the designator inside it. It is
			// asked for with expansion off so the line reaches the
			// builtin instead of being expanded by the reader.
			name: "history -p reports its own way",
			script: `set -o history
echo one two
history -p '!!:z'
history -p '!!:9'
history -p '!nosuchprefix'
history -p '!!'
echo end
`,
			oracle: "history expansion failed",
		},
		{
			// An unquoted `&` in a substitution's replacement is the text
			// that matched, and `\&` is a literal one. The results are
			// double-quoted because a surviving `&` is a shell operator,
			// and a backgrounded echo would make the case's own ordering
			// a race rather than an answer.
			name: "an ampersand in a replacement",
			script: `set -o history -H
echo foo.c foo.o
!!:gs/foo/x&/
echo aXb
echo A "!!:s/X/[&]/"
echo aXb
echo B "!!:s/X/[\&]/"
echo end
`,
			oracle: "echo xfoo.c xfoo.o",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "s.sh"), []byte(tc.script), 0o600); err != nil {
				t.Fatal(err)
			}
			bashOut, bashCode := runInDir(t, dir, bash, "./s.sh")
			koiOut, koiCode := runInDir(t, dir, koi, "./s.sh")
			if bashOut != koiOut || bashCode != koiCode {
				t.Errorf("differs from bash\n  bash: %q (exit %d)\n   koi: %q (exit %d)",
					bashOut, bashCode, koiOut, koiCode)
			}
			if !strings.Contains(bashOut, tc.oracle) {
				t.Errorf("the oracle's output lacks %q, so this case cannot detect a regression: %q",
					tc.oracle, bashOut)
			}
		})
	}
}

// TestHistCharsMovesTheExpansionCharacter covers $histchars (#695), whose
// sharper half is the *second* one: after the variable moves the
// expansion character an ordinary `!!` has to stop being an expansion, so
// a shell that only learned the new character still expands a line bash
// leaves alone.
func TestHistCharsMovesTheExpansionCharacter(t *testing.T) {
	if testing.Short() {
		t.Skip("differential history expansion skipped in -short")
	}
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	for _, tc := range []struct{ name, script, oracle string }{
		{
			name: "all three characters move",
			script: `set -o history -H
histchars=',;%'
echo alpha
,,
echo aXc
;X;Y
echo ok % ,1200
echo beta
!!
echo end
`,
			oracle: "!!: command not found",
		},
		{
			// A shorter value moves only the characters it names. bash
			// leaves the rest at whatever they were, which is the default
			// until something else has set them.
			name: "a shorter value leaves the rest alone",
			script: `set -o history -H
histchars=','
echo alpha
,,
echo aXc
^X^Y
echo end
`,
			oracle: "echo aYc",
		},
		{
			// An empty value takes the expansion character away
			// altogether, which turns expansion off rather than leaving
			// it at its default.
			name: "an empty value turns expansion off",
			script: `set -o history -H
histchars=''
echo alpha
!!
echo end
`,
			oracle: "!!: command not found",
		},
		{
			// Unsetting the variable restores bash's own three.
			name: "unset restores the defaults",
			script: `set -o history -H
histchars=','
echo alpha
unset histchars
echo beta
!!
echo end
`,
			oracle: "echo beta",
		},
		{
			// The variable is only spelled in lower case; the upper-case
			// name is an ordinary variable.
			name: "HISTCHARS is not the variable",
			script: `set -o history -H
HISTCHARS=','
echo alpha
,,
!!
echo end
`,
			oracle: ",,: command not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "s.sh"), []byte(tc.script), 0o600); err != nil {
				t.Fatal(err)
			}
			bashOut, bashCode := runInDir(t, dir, bash, "./s.sh")
			koiOut, koiCode := runInDir(t, dir, koi, "./s.sh")
			if bashOut != koiOut || bashCode != koiCode {
				t.Errorf("differs from bash\n  bash: %q (exit %d)\n   koi: %q (exit %d)",
					bashOut, bashCode, koiOut, koiCode)
			}
			if !strings.Contains(bashOut, tc.oracle) {
				t.Errorf("the oracle's output lacks %q, so this case cannot detect a regression: %q",
					tc.oracle, bashOut)
			}
		})
	}
}

// TestHistoryRecordsALineThatRunsNothing covers #693: bash records a line
// as it *reads* it, so a comment line is an entry and koi's statement
// hook never saw one — which put every later listing number out by the
// number of comments above it.
//
// Every case prints a `history` listing, because the numbers are the
// interesting part: a shell that recorded the comment text but numbered
// from somewhere else fails on the numbers.
func TestHistoryRecordsALineThatRunsNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("differential history recording skipped in -short")
	}
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	for _, tc := range []struct{ name, script, oracle string }{
		{
			// The distinction is real rather than "record every line": a
			// comment is an entry, a whitespace-only line is an entry,
			// and a wholly empty line is not.
			name:   "a comment is an entry and an empty line is not",
			script: "set -o history\necho alpha\n# a comment line\n   # indented\n\necho beta\n\t\necho gamma; # trailing\nhistory\n",
			oracle: "# a comment line",
		},
		{
			// Recording is decided when the line is *read*, so a license
			// header above `set -o history` is not an entry, the line
			// that turns recording off is, and the line that turns it
			// back on is not.
			name:   "eligibility is decided at read time",
			script: "# header above the option\n# second header\nset -o history\necho a\nset +o history\n# while off\necho b\nset -o history\n# while on again\necho c\nhistory\n",
			oracle: "# while on again",
		},
		{
			// A comment *inside* a multi-line command is dropped from the
			// entry rather than added as one, and the boundary it stood
			// at joins with a newline — measured byte for byte with
			// `history -w`, which is why the listing here spans lines.
			name:   "a comment inside a compound command",
			script: "set -o history\necho a\nfor i in 1 2; do\n# inner comment\n\techo $i\ndone\nif true\nthen\n   # another\n   echo c\nfi\nfor j in 1\ndo\n\n\techo blank-above\ndone\nhistory\n",
			oracle: "for i in 1 2; do",
		},
		{
			// A line with code and a trailing comment is kept, and what
			// follows it joins with a newline for the same reason: a `; `
			// written after a comment would be inside it.
			name:   "a line with code and a comment",
			script: "set -o history\nfor i in 1; do\n\techo $i\t# tail comment\n\techo x\ndone\necho 'a#b' \\\n  tail\nhistory\n",
			oracle: "# tail comment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "s.sh"), []byte(tc.script), 0o600); err != nil {
				t.Fatal(err)
			}
			bashOut, bashCode := runInDir(t, dir, bash, "./s.sh")
			koiOut, koiCode := runInDir(t, dir, koi, "./s.sh")
			if bashOut != koiOut || bashCode != koiCode {
				t.Errorf("differs from bash\n  bash: %q (exit %d)\n   koi: %q (exit %d)",
					bashOut, bashCode, koiOut, koiCode)
			}
			if !strings.Contains(bashOut, tc.oracle) {
				t.Errorf("the oracle's output lacks %q, so this case cannot detect a regression: %q",
					tc.oracle, bashOut)
			}
		})
	}
}

// TestHistoryOnStandardInput covers #694: `koi < script` and
// `cat script | koi` are the reading path that is not an
// interp.ScriptReader, so neither history expansion nor the ambient
// recording it reads from reached it — and the failure was silent, `!!`
// becoming a command name and answering 127.
//
// Both arrangements are driven rather than one assumed from the other,
// which is #516's rule for this loop.
func TestHistoryOnStandardInput(t *testing.T) {
	if testing.Short() {
		t.Skip("differential history expansion skipped in -short")
	}
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	const script = `set -o history -H
echo alpha
!!
# a comment line
echo beta
!-1
echo A !?alpha?:1
history
`
	for _, piped := range []bool{false, true} {
		name := "redirected"
		if piped {
			name = "piped"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bashOut, bashErr, bashCode := runOnStdin(t, bash, script, piped)
			koiOut, koiErr, koiCode := runOnStdin(t, koi, script, piped)
			if bashOut != koiOut || bashCode != koiCode {
				t.Errorf("stdout differs from bash\n  bash: %q (exit %d)\n   koi: %q (exit %d)",
					bashOut, bashCode, koiOut, koiCode)
			}
			// The echo of an expansion is a diagnostic and goes to
			// stderr, so asserting stdout alone would pass for a shell
			// that expanded silently.
			if bashErr != koiErr {
				t.Errorf("stderr differs from bash\n  bash: %q\n   koi: %q", bashErr, koiErr)
			}
			// Non-vacuity: two shells that expanded nothing agree with
			// each other, so the answer has to carry the expansion by
			// name — and the echo of one goes to stderr, which is also
			// where the old failure printed `!!: command not found`.
			if !strings.Contains(koiErr, "echo alpha") {
				t.Errorf("no expansion was echoed, so nothing expanded: %q", koiErr)
			}
			if strings.Contains(koiErr, "command not found") {
				t.Errorf("an event reached the exec seam as a command name: %q", koiErr)
			}
			if !strings.Contains(koiOut, "# a comment line") {
				t.Errorf("the comment line is not a history entry: %q", koiOut)
			}
		})
	}
}
