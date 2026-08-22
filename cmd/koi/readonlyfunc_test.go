//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A readonly function under a *script file*, which is where two of #615's
// answers live and neither is visible from inside interp's table.
//
// The location is the first: interp's cases are fed a command string, so
// they carry no `file: line N: ` prefix at all (#571), and the line a
// redefinition names is the one the definition *ends* on rather than the
// one it starts on — a four-line body reported at its closing brace,
// measured off bash rather than derived from the one-line case. The
// carry-on is the second: every refusal here answers 1 and lets the rest
// of the line and the rest of the file run, so `echo B` after an inline
// redefinition is what separates this from the plain assignment's
// abandonment (#308).
//
// Differential, because both of those are bash's answers rather than
// koi's opinion, and the script is bash's own errors.tests lines 66–85
// with the three neighbors that issue leaves out: `unset -x`'s missing
// usage line (#577), `unalias`'s location, and the posix `for`/`select`
// shape.
const readonlyFuncScript = `func()
{
	echo orig
}
readonly -f func
declare -Fr
func()
{
	echo bar
}
unset -f func
declare -fr func
declare -f +r func
echo A; func() { echo inline; }; echo B
func
echo "end=$?"
`

func TestReadonlyFunctionsMatchBashInAScriptFile(t *testing.T) {
	if testing.Short() {
		t.Skip("differential readonly-function cases skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "s.sh"), []byte(readonlyFuncScript), 0o700); err != nil {
		t.Fatal(err)
	}

	// Run by a path relative to the directory both shells start in, so
	// they name the same file: bash prints a script's path as written.
	bashOut, bashCode := runInDir(t, tmp, bash, "./s.sh")
	koiOut, koiCode := runInDir(t, tmp, koi, "./s.sh")

	if bashOut != koiOut || bashCode != koiCode {
		t.Errorf("readonly functions differ from bash\n  bash: %q (exit %d)\n  koi:  %q (exit %d)",
			bashOut, bashCode, koiOut, koiCode)
	}
	// Two shells agreeing on an answer that contains none of the things
	// under test would pass while proving nothing. The multi-line
	// refusal has to be located at the closing brace on line 10, the
	// body has to have survived every refusal, and the statement after
	// the inline redefinition has to have run.
	for _, want := range []string{
		"./s.sh: line 10: func: readonly function",
		"./s.sh: line 11: unset: func: cannot unset: readonly function",
		"./s.sh: line 13: declare: func: readonly function",
		"\nA\n",
		"\nB\n",
		"\norig\n",
	} {
		if !strings.Contains(bashOut, want) {
			t.Errorf("the oracle did not produce %q, so this case cannot detect its absence: %q", want, bashOut)
		}
	}
}
