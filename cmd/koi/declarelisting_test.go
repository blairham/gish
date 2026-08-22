//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// `declare -a` lists the shell's own arrays, in a script and in a
// sourced file (#616).
//
// This lives in cmd/koi rather than in interp's own differential table
// because that table feeds bash on *standard input*, where BASH_SOURCE
// holds the path of the bash binary and BASH_LINENO counts from the
// string — so a case naming the script file cannot be compared there.
// A script file is also the sharpest shape for the listing: BASH_SOURCE,
// BASH_LINENO and FUNCNAME all have content, and they have *different*
// content at the top level, inside a function, inside a sourced file,
// and inside a function defined by one.
//
// The listing is filtered to what koi has: bash also lists BASH_ARGC and
// BASH_ARGV, which koi does not implement at all (#637), and it reports
// DIRSTACK and GROUPS as empty until something reads them, which koi
// answers from the live state (#689).
const declListScript = `mine=(one two)
snap() { grep -E ' (BASH_SOURCE|BASH_LINENO|FUNCNAME|mine)(=|$)'; }
echo "== top"
declare -a | snap
f() {
	echo "== in f"
	declare -a | snap
}
f
. ./lib.sh
echo "== a subscript is not a name"
ro=(1)
readonly ro[0]
echo "rc=$?"
export ex[1]=1
echo "rc=$?"
echo "== declare -p on a computed array"
declare -p FUNCNAME
declare -p BASH_SOURCE
`

const declListLib = `echo "== in lib"
declare -a | snap
libf() {
	echo "== in lib fn"
	declare -a | snap
}
libf
`

func TestDeclareListsTheShellsOwnArrays(t *testing.T) {
	if testing.Short() {
		t.Skip("differential listing skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	tmp := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("main.sh", declListScript)
	write("lib.sh", declListLib)

	// cd first so both shells are handed the same relative path: what
	// BASH_SOURCE holds is the path as *written*, not as resolved, and
	// that is half of what this test is checking.
	script := "cd " + tmp + "\n. ./main.sh 2>&1"

	r := compat.Run(context.Background(), bash, koi, compat.Case{
		Name: "declare -a lists the shell's own arrays", Script: script,
	})
	if !r.Pass {
		t.Errorf("listing differs from bash (%s)\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
			r.Reason, r.BashOut, r.BashCode, r.KoiOut, r.KoiCode)
	}
	// Two shells agreeing on an empty listing would pass while proving
	// nothing, and so would a listing with none of the computed arrays
	// in it — which is the bug. Each marker is a value the shell can
	// only produce from its own execution context.
	for _, want := range []string{
		`declare -a BASH_SOURCE=([0]="./main.sh")`,
		`declare -a FUNCNAME=([0]="f" [1]="source")`,
		`declare -a BASH_SOURCE=([0]="./lib.sh" [1]="./main.sh")`,
		`declare -a BASH_LINENO=([0]="7" [1]="10" [2]="2")`,
		`declare -a FUNCNAME=([0]="libf" [1]="source" [2]="source")`,
		"readonly: `ro[0]': not a valid identifier",
	} {
		if !strings.Contains(r.BashOut, want) {
			t.Errorf("the oracle never produced %q, so this case cannot detect its absence:\n%s", want, r.BashOut)
		}
	}
	// The other half: a listing that contains everything satisfies every
	// containment check above, so assert an ordinary variable is *not*
	// in `declare -a`. `mine` is there to prove the filter itself works.
	if !strings.Contains(r.KoiOut, `declare -a mine=([0]="one" [1]="two")`) {
		t.Errorf("koi's listing has no ordinary array in it, so the filter is hiding everything:\n%s", r.KoiOut)
	}
	if strings.Contains(r.KoiOut, "snap") {
		t.Errorf("`declare -a` listed a function name, so it is not filtering by kind:\n%s", r.KoiOut)
	}
}
