//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A subscript is read when the assignment runs (#582), differentially
// against real bash.
//
// interp's own table covers the messages; these are the two things it
// structurally cannot cover, because in a `-c` string the whole input is
// one unit:
//
//   - **the line is what a bad subscript costs** — a statement before it
//     on the same line has run, and a statement after it does not,
//     which is #469's abandonment rather than an ordinary error;
//   - **what the variable is left holding** — an append keeps its base
//     while a plain assignment is left empty, and every element of the
//     rejected assignment is discarded rather than only the bad one.
//
// Run as script files for the same reason, and `declare -p` reads the
// state back rather than trusting an expansion, since that is the form
// bash prints and diffs cleanly.
var subscriptCases = []struct {
	name string
	body string
	// needs is a string the oracle must print, so a case cannot pass by
	// two shells agreeing on nothing. It is the diagnostic for the
	// refusals and the *result* for the one case that is not a refusal.
	needs string
}{
	{
		// Left of the `=`: the rest of the line is gone, the next line
		// runs, and `$?` on that next line is 1.
		name: "an empty subscript costs the line",
		body: "b=(zero one)\n" +
			"echo pre; b[]=x; echo \"same=$?\"\n" +
			"echo \"after=$?\"\n" +
			"declare -p b\n",
		needs: "b[]: bad array subscript",
	},
	{
		// The whole-array subscripts are not indices, and bash's word
		// for that is not an arithmetic error.
		name: "a whole-array subscript is not an index",
		body: "b=(zero one)\n" +
			"b[*]=x; echo \"after=$?\"\n" +
			"b[@]=x; echo \"after=$?\"\n" +
			"declare -p b\n",
		needs: "b[*]: bad array subscript",
	},
	{
		// A negative index past the start, named as written.
		name: "a negative index out of range",
		body: "c=(one two)\n" +
			"echo pre; c[-9]=x; echo \"same=$?\"\n" +
			"declare -p c\n",
		needs: "c[-9]: bad array subscript",
	},
	{
		// bash has no list-to-member assignment; zsh does, which is why
		// the parser keeps the shape and the interpreter refuses it.
		name: "a list assigned to a member",
		body: "echo pre; d[7]=(a b); echo \"same=$?\"\n" +
			"declare -A m\n" +
			"m[k]=(a b); echo \"after=$?\"\n" +
			"declare -p d m 2>/dev/null\n",
		needs: "cannot assign list to array member",
	},
	{
		// An element's subscript: the diagnostic names the element, and
		// the *whole* compound assignment is discarded.
		name: "an element's subscript discards the assignment",
		body: "d=([]=abcde [1]=\"test test\" [*]=last [-65]=negative )\n" +
			"echo \"after=$?\"\n" +
			"declare -p d\n" +
			"e=([*]=last); echo \"after=$?\"\n" +
			"declare -p e\n",
		needs: "[]=abcde: bad array subscript",
	},
	{
		// An append keeps what the variable already held.
		name: "an append keeps its base",
		body: "f=(keep)\n" +
			"f+=([]=lost); echo \"after=$?\"\n" +
			"declare -p f\n",
		needs: `declare -a f=([0]="keep")`,
	},
	{
		// The blank subscript is not the empty one: whitespace is an
		// empty arithmetic expression, so it is index 0 — for a write
		// and for a read.
		name: "a blank subscript is index zero",
		body: "g=(first second)\n" +
			"g[  ]=written; echo \"after=$?\"\n" +
			"echo \"read=[${g[   ]}]\"\n" +
			"declare -p g\n",
		needs: "read=[written]",
	},
	{
		// `declare` reports and carries on to the next name, where a
		// plain assignment abandons the line.
		name: "declare answers one and keeps going",
		body: "declare a[]=x c=2; echo \"same=$? c=[$c]\"\n" +
			"local_probe() { local h[]=1; echo \"in=$?\"; }\n" +
			"local_probe\n",
		needs: "c=[2]",
	},
}

func TestSubscriptVerdictsMatchBash(t *testing.T) {
	if testing.Short() {
		t.Skip("differential subscript verdicts skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	for _, tc := range subscriptCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "s.sh"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			bashOut, bashCode := runInDir(t, dir, bash, "./s.sh")
			koiOut, koiCode := runInDir(t, dir, koi, "./s.sh")
			if bashOut != koiOut || bashCode != koiCode {
				t.Errorf("%s differs from bash\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
					tc.name, bashOut, bashCode, koiOut, koiCode)
			}
			// Two shells agreeing on nothing would pass every case
			// here, so the oracle has to have produced the diagnostic
			// this is all about.
			if !strings.Contains(bashOut, tc.needs) {
				t.Errorf("%s: the oracle never printed %q, so the case proves nothing: %q",
					tc.name, tc.needs, bashOut)
			}
		})
	}
}
