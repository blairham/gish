//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// History expansion in a script (#559).
//
// These run a real *file*, which is where the rule this issue is about is
// visible at all: the unit is the physical line, so `set -H` affects the
// next line and never its own, and a `-c` string cannot show that. The
// harness is #571's — a script written into the case's own directory and
// run by a relative path, so both shells name the same file in a
// diagnostic.
//
// Differential, because every answer here was measured off bash rather
// than reasoned about: which stream the echo goes to, whether the rest of
// a refused line runs, what `$?` is afterwards, and the line numbering
// after a line expansion emptied.
func TestHistoryExpansionInAScript(t *testing.T) {
	if testing.Short() {
		t.Skip("differential history expansion skipped in -short")
	}
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	for _, tc := range []struct {
		name, script string
		// oracle is a string bash's own output must contain, so a case
		// where both shells agree on nothing useful cannot pass quietly.
		oracle string
	}{
		{
			// The gate is two options and not one, which is measured in
			// both directions: `set -H` alone expands nothing, and
			// turning history back off stops the expansion rather than
			// only stopping the recording.
			name: "the gate is histexpand and history together",
			script: `echo alpha
set -H
!!
echo beta
set -o history
echo gamma
!!
set +o history
echo delta
!!
echo "done=$?"
`,
			oracle: "echo gamma",
		},
		{
			// #450's rule, re-measured here rather than inherited: the
			// option is read before the line runs, so a line cannot
			// expand itself — while a continuation line of a compound
			// command is its own line and does expand.
			name: "the unit is the physical line",
			script: `set -o history
set -H; echo same-line !!
echo alpha
for i in 1; do
  !!
done
echo beta
if true
then
  !!
fi
`,
			oracle: "same-line !!",
		},
		{
			// Quoting decides expansion because expansion happens before
			// parsing: single quotes inhibit it, double quotes do not,
			// and a backslash quotes the character itself.
			name: "quotes and backslashes",
			script: `set -o history -H
echo alpha
echo '!!' one
echo "!!" two
echo back\!\!slash
echo '!!' !!
`,
			oracle: `echo "echo '!!' one" two`,
		},
		{
			// A here-document's body is the document's text. Both
			// delimiter forms, because the quoting of the delimiter is
			// what decides *expansion* of the body and decides nothing
			// here.
			name: "a here-document body is not expanded",
			script: `set -o history -H
echo alpha
cat <<EOF
body !!
EOF
cat <<'EOF'
quoted body !!
EOF
cat <<-EOF
	tabbed body !!
EOF
echo tail
`,
			oracle: "body !!",
		},
		{
			// The three shell shapes that are not events: a negated
			// bracket expression, an indirect expansion, and everything
			// after the history comment character.
			name: "the shapes that are not events",
			script: `set -o history -H
case p in
[!A-Z]) echo bracket ok;;
esac
v1=vv; v2=v1
echo indirect ${!v2}
echo comment ok # !1200
echo x#y ok
true;# !1200
echo end
`,
			oracle: "bracket ok",
		},
		{
			// A refusal costs the line and leaves `$?` alone — and costs
			// bash's line counter a line too, so everything numbered
			// after it moves. The unbound command at the end is there to
			// print a number from the other diagnostic family.
			name: "a refused expansion costs the line",
			script: `set -o history -H
echo pre
echo mid; !nosuchprefix; echo tail
echo "status=$?"
false
!nosuchprefix
echo "status=$?"
echo "lineno=$LINENO"
nosuch_command_after_a_refusal
`,
			oracle: "event not found",
		},
		{
			// `:p` asks to be shown rather than run, and it is shown
			// once. It leaves the input the way a refused line does,
			// which is why it moves the numbering too.
			name: "the p modifier shows without running",
			script: `set -o history -H
echo one two three
echo A !!:2:p
echo "lineno=$LINENO"
!!:p
echo end
`,
			oracle: "echo A two",
		},
		{
			// What the shell recalls and what `history` prints are the
			// same list, and the entry it keeps is the *expanded* line.
			name: "the expanded line is what is recorded",
			script: `set -o history -H
echo alpha
!!
history -p '!!'
history
`,
			oracle: "echo alpha",
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

// The two halves that a fix which simply turned expansion on would pass.
//
// A test asserting only that `!!` expands is satisfied by a shell that
// expands unconditionally, which would be a script's meaning changed by a
// character that has always been an ordinary word in one — so the default
// and the way back off are asserted by name, not only differentially.
func TestHistoryExpansionIsOffUntilAScriptAsks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	t.Parallel()
	koi := buildKoi(t)

	for _, tc := range []struct{ name, script string }{
		{
			"never asked for",
			"echo alpha\n!!\necho end\n",
		},
		{
			"asked for and turned back off",
			"set -o history -H\necho alpha\nset +H\n!!\necho end\n",
		},
		{
			// The other half of the two-option gate: histexpand on its
			// own is not enough, so a shell reading only that bit fails
			// here.
			"histexpand without history",
			"set -H\necho alpha\n!!\necho end\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "s.sh"), []byte(tc.script), 0o600); err != nil {
				t.Fatal(err)
			}
			out, _ := runInDir(t, dir, koi, "./s.sh")
			if !strings.Contains(out, "!!: command not found") {
				t.Errorf("`!!` was not left an ordinary word: %q", out)
			}
			if strings.Count(out, "alpha") != 1 {
				t.Errorf("the previous command ran again, so the line was expanded: %q", out)
			}
			if !strings.Contains(out, "end") {
				t.Errorf("the script did not carry on past the line: %q", out)
			}
		})
	}
}
