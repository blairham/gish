package main

import (
	"testing"
)

// Ambient history recording (#277): under `set -o history` the shell
// records each command as it executes, which is what makes `history`,
// `fc -l`, `history -p '!!'` and HISTSIZE mean something in a script.
//
// Every case runs the same script through real bash and through koi and
// requires identical stdout and exit status — bash is the oracle, and
// each script ends with `history` (or fc) so the recorded list itself is
// what is compared. The joining rules these scripts exercise are raw
// source facts (a tab inside a loop body survives; a heredoc keeps its
// newlines), which is why the recorder slices source rather than
// pretty-printing the tree.
func TestAmbientHistoryMatchesBash(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	cases := []struct {
		name   string
		script string
	}{
		{"records before executing so history lists itself", `set -o history
echo a >/dev/null
history`},
		{"one line of statements is one entry", `set -o history
echo a >/dev/null; echo b >/dev/null
history`},
		{"a loop keeps its tab and joins on keywords", "set -o history\nfor x in one two three\ndo\n\t:\ndone\nif true; then\n  echo hi >/dev/null\nfi\nhistory"},
		{"continuations splice and operators join with spaces", `set -o history
echo one \
two >/dev/null
true &&
false
echo x |
cat >/dev/null
history`},
		{"case and brace groups", `set -o history
case a in
a) : ;;
esac
{
echo grp >/dev/null
}
history`},
		{"heredoc keeps newlines and its trailing one", `set -o history
cat <<HD >/dev/null
body line
HD
history`},
		{"open quotes and command substitutions keep newlines, arrays do not", `set -o history
echo 'two
lines' >/dev/null
a=(1
2)
x=$(echo a
echo b)
history`},
		{"a trailing command folds into the compound's entry", `set -o history
for x in 1
do
:
done; echo tail >/dev/null
history`},
		{"source and eval record only the invoking line", `mkdir -p d277
printf 'echo sourced >/dev/null\n' > d277/lib.sh
set -o history
. ./d277/lib.sh
eval 'echo evaled >/dev/null'
history`},
		{"set +o history pauses and is itself the last entry", `set -o history
echo on1 >/dev/null
set +o history
echo off >/dev/null
set -o history
echo on2 >/dev/null
history`},
		{"HISTCONTROL ignoreboth: a space skips, a tab does not, dups collapse", "set -o history\nHISTCONTROL=ignoreboth\n echo leadingspace >/dev/null\n\techo tabbed >/dev/null\necho dup >/dev/null\necho dup >/dev/null\nhistory"},
		{"HISTIGNORE patterns and ampersand", `set -o history
HISTIGNORE='&:history*:fc*'
echo x >/dev/null
echo x >/dev/null
echo y >/dev/null
history
fc -l`},
		{"HISTSIZE trims live and the numbering keeps advancing", `set -o history
HISTSIZE=3
echo a >/dev/null
echo b >/dev/null
echo c >/dev/null
echo d >/dev/null
history`},
		{"a shrinking assignment renumbers one lower", `set -o history
history -c
echo a >/dev/null
echo b >/dev/null
echo c >/dev/null
HISTSIZE=2
history`},
		{"shrink then grow", `set -o history
history -c
echo a >/dev/null
echo b >/dev/null
HISTSIZE=2
HISTSIZE=10
echo c >/dev/null
history`},
		{"history -c restarts the numbering", `set -o history
echo a >/dev/null
echo b >/dev/null
history -c
echo c >/dev/null
history`},
		{"history -s replaces its own line and is filtered", `set -o history
HISTCONTROL=ignoreboth
HISTIGNORE='skipme*'
history -s ' spaced entry'
history -s 'skipme too'
history -s 'kept entry'
history`},
		{"history -p removes its own line and expands the previous", `set -o history
echo a >/dev/null
history -p '!!'
history`},
		{"fc -l shares the advancing numbers after a trim", `set -o history
HISTSIZE=3
echo a >/dev/null
echo b >/dev/null
echo c >/dev/null
echo d >/dev/null
fc -l`},
		{"recording stays off until asked", `echo before >/dev/null
set -o history
history`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Neither shell may read the developer's real history: a
			// scratch HOME and cwd for both, same rule as #260.
			compareStdoutEnv(t, bashBin, koiBin, "cd "+dir+"\n"+tc.script, nil)
		})
	}
}

// `set -o history` used to be refused ("cannot turn history on"); it is
// a real switch now and the listing tracks it. shopt -q is a separate,
// pre-existing gap, so the probe reads `set -o` the way scripts do.
func TestSetOptionHistoryToggles(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)
	compareStdout(t, bashBin, koiBin, `set -o history
set -o | grep ' history' | tr -s ' \t' ' '
set +o history
set -o | grep ' history' | tr -s ' \t' ' '`)
}
