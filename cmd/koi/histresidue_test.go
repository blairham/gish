//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The history residue #726 measured and filed: the word designators'
// tokenizer (#709), `history -d`'s three shapes (#710) and `fc`'s
// re-execute form (#711).
//
// Differential against real bash over a real *file*, for the reason
// histevents_test.go's cases are: history's unit is the physical line, so
// an entry is only what it is once a line has been read — a `-c` string
// is one entry however many commands are in it, which hides every
// numbering rule these cases are about. `TestRunnerRunConfirm` is not the
// home for any of this either: history expansion lives in internal/repl
// and never runs under interp, so a case in that table would assert
// nothing.

// histCase is one differential script plus a string bash's own output
// must contain, so a case where the two shells agree on nothing useful
// cannot pass quietly.
type histCase struct{ name, script, oracle string }

func runHistCases(t *testing.T, cases []histCase) {
	t.Helper()
	if testing.Short() {
		t.Skip("differential history cases skipped in -short")
	}
	koi, bash := buildKoi(t), bashBin(t)
	for _, tc := range cases {
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

// TestHistoryWordTokenizer covers #709: a history entry is split by
// readline's own tokenizer, not by the shell's word splitting, so a
// metacharacter is a word of its own.
func TestHistoryWordTokenizer(t *testing.T) {
	t.Parallel()
	runHistCases(t, []histCase{
		{
			// The case histexp.tests carries deliberately, with a comment
			// saying bash through 4.3 got it wrong: the operator and its
			// target are two words, so they rejoin with a space in them.
			name: "a redirection operator is its own word",
			script: `set -o history -H
shopt a b c d 2>/dev/null
echo M
echo !-2*
echo end
`,
			oracle: "a b c d 2> /dev/null",
		},
		{
			// The invisible half, and the one that changes meaning: the
			// word *count* moves, so `:$` is the target rather than the
			// operator and target together, and a numbered designator
			// after the operator is a different word.
			name: "the operator changes the word count",
			script: `set -o history -H
shopt a b c d 2>/dev/null
echo M
echo LAST !-2:$
echo N
echo SIXTH !-4:6
echo end
`,
			oracle: "SIXTH /dev/null",
		},
		{
			// A file descriptor sticks to the operator it precedes, and a
			// duplicating form takes its digits with it — neither of which
			// falls out of "break on metacharacters".
			name: "descriptors belong to their operator",
			script: `set -o history -H
echo one>&2 two
echo M
echo "A !-2:1 B !-2:2 C"
echo end
`,
			oracle: "A one B >&2 C",
		},
		{
			// The half koi already had right, kept as a case because the
			// tokenizer is where it could stop being right: a command or
			// process substitution is one word however many spaces are in
			// it, and so is an extended glob.
			name: "substitutions stay one word",
			script: `set -o history -H
shopt -s extglob
echo /$(echo tmp)/Step1 >/dev/null
echo !:*
echo /<(echo tmp)/Step1 >/dev/null
echo !:*
echo /+(one|two)/Step1 >/dev/null
echo !:*
echo end
`,
			oracle: "/<(echo tmp)/Step1 > /dev/null",
		},
		{
			// A control operator is a word, and a paren is a word of its
			// own — which is what keeps `!!:0` the command name on a line
			// that starts with one.
			name: "control operators are words",
			script: `set -o history -H
echo a&&echo b
echo M
echo W1 !-2:1 W3 !-2:3
echo end
`,
			oracle: "W1 a W3 echo",
		},
	})
}

// TestHistoryDeleteShapes covers #710: `history -d` takes a range and a
// count-back as well as a single offset, and "not a number" and "not a
// position I have" are two different diagnostics.
func TestHistoryDeleteShapes(t *testing.T) {
	t.Parallel()
	runHistCases(t, []histCase{
		{
			name: "a range deletes every entry in it",
			script: `set -o history
history -c
echo a; echo b; echo c; echo d; echo e
history -d 2-4; echo "st=$?"
history
`,
			oracle: "echo e",
		},
		{
			// Either end may count back from the newest, which is what
			// makes `-1` the entry just recorded — the `history -d` line
			// itself.
			name: "either end may count back",
			script: `set -o history
history -c
echo a; echo b; echo c
history -d 4--1; echo "st=$?"
history
echo ---
history -c
echo p; echo q
history -d -1; echo "stlast=$?"
history
`,
			oracle: "echo a",
		},
		{
			// "not a number" and "out of range" are different answers, and
			// which one a shape gets is not guessable: `5-0xaf` is the
			// second, because bash reads numbers in base ten and `0xaf`
			// stops at the `x` rather than failing to be a number.
			name: "invalid number and out of range are different",
			script: `set -o history
history -c
echo a; echo b
history -d 16-40; echo "st1=$?"
history -d 1-200; echo "st2=$?"
history -d -20-50; echo "st3=$?"
history -d 1--50; echo "st4=$?"
history -d 5-0xaf; echo "st5=$?"
history -d @42; echo "st6=$?"
history -d ''; echo "st7=$?"
history -d 2-; echo "st8=$?"
history -d 0; echo "st9=$?"
history
`,
			oracle: "5-0xaf: history position out of range",
		},
		{
			// An end before its start deletes nothing and answers 1 with
			// **no message at all**, which is readline refusing the pair
			// and the builtin passing the result on unexamined.
			name: "a backwards range fails silently",
			script: `set -o history
history -c
echo a; echo b; echo c
history -d 3-1; echo "st=$?"
history
`,
			oracle: "st=1",
		},
		{
			// The surrounding-whitespace and leading-plus forms, which
			// `strconv.Atoi` refuses and bash's own number reader does not.
			name: "whitespace and a leading plus are numbers",
			script: `set -o history
history -c
echo a; echo b; echo c
history -d ' 2 '; echo "st=$?"
history -d +2; echo "stplus=$?"
history
`,
			oracle: "echo a",
		},
	})
}

// TestFCReExecute covers #711's re-execute form: `fc -s`, the `r` alias
// everybody defines for it, and `fc -e -` which is the same thing.
func TestFCReExecute(t *testing.T) {
	t.Parallel()
	runHistCases(t, []histCase{
		{
			// The whole point of the form: the previous command runs
			// again, echoed on stderr first, and it is the *previous* one
			// rather than the fc line itself.
			name: "a bare fc -s re-runs the previous command",
			script: `set -o history
history -c
echo alpha
fc -s
echo end
`,
			oracle: "echo alpha",
		},
		{
			// Substitutions apply to the whole command, globally, and in
			// the order they were written.
			name: "pat=rep substitutes globally",
			script: `set -o history
history -c
echo z z z
fc -s z=Y
echo p1 p2 p3
fc -s p1=q1 p2=q2
echo end
`,
			oracle: "echo Y Y Y",
		},
		{
			// The status is the re-executed command's, which is the half
			// that could not work if the builtin ran it: a CallHandler
			// error is fatal to the shell, so this comes back through the
			// interpreter's own path.
			name: "the status is the command's",
			script: `set -o history
history -c
(exit 42)
fc -s -1; echo "st=$?"
echo end
`,
			oracle: "st=42",
		},
		{
			// A specification can name an entry by prefix, by absolute
			// number, or count back — and one that names nothing is "no
			// command found" rather than silence.
			name: "specifications and the ones that resolve to nothing",
			script: `set -o history
history -c
echo aa ab ac
echo bb
fc -s echo=printf' ' aa; echo "st1=$?"
fc -s zzz; echo "st2=$?"
fc -s -0; echo "st3=$?"
echo end
`,
			oracle: "no command found",
		},
		{
			// `fc -e -` is the same form spelled the other way, and an
			// out-of-range specification is *not* an error for it — POSIX
			// says so and history5.sub tests it by name.
			name: "fc -e - is the same form and clamps its range",
			script: `set -o history
history -c
echo a
echo b
echo c
fc -e - 48; echo "st1=$?"
fc -s -- -42; echo "st2=$?"
echo end
`,
			oracle: "st1=0",
		},
		{
			// The re-executed command replaces the `fc -s` line in the
			// history, which is what makes `r` repeatable rather than
			// self-referential.
			name: "the command replaces the fc line in history",
			script: `set -o history
shopt -s expand_aliases
alias r="fc -s"
history -c
echo one
r
history
`,
			oracle: "echo one",
		},
		{
			// The listing form's specifications go through the same
			// resolver, which is where "too many arguments" used to come
			// from: a non-numeric first operand is a prefix search, and
			// anything past the second operand is ignored.
			name: "listing specifications resolve the same way",
			script: `set -o history
history -c
echo a
echo b
echo c
fc -l one=two three=four 502; echo "st1=$?"
fc -l 0
fc -l -0
fc -l 2 1
echo end
`,
			oracle: "no command found",
		},
		{
			// `-0` is not a position for the editor form, and bash reports
			// that before it ever reaches an editor — so this is the one
			// editing-form diagnostic koi answers without implementing the
			// editor.
			name: "a bad specification is reported before the editor",
			script: `set -o history
history -c
echo a
fc -0; echo "st=$?"
echo end
`,
			oracle: "history specification out of range",
		},
	})
}
