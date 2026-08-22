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
	{
		// A subscript that will not read as arithmetic is an assignment
		// that does not happen (#564). koi printed the error and then
		// wrote the value over element zero anyway, which is the worst
		// of both: a diagnostic and a mutation. stderr is dropped here
		// because koi's arithmetic wording still lacks bash's
		// error-token tail (#598); the line's control flow and what the
		// array is left holding are what this case is about.
		name: "a non-arithmetic subscript assigns nothing",
		body: "exec 2>/dev/null\n" +
			"declare -a a\n" +
			"echo pre; a[hello world]=1; echo \"same=$?\"\n" +
			"echo \"after=$?\"\n" +
			"declare -p a\n",
		needs: "declare -a a\n",
	},
	{
		// The same subscript inside a compound assignment costs *every*
		// element rather than the one that was wrong, and an append is
		// left with the base it was appending to.
		name: "a non-arithmetic element subscript costs the assignment",
		body: "exec 2>/dev/null\n" +
			"declare -a d=(keep)\n" +
			"echo pre; d=([hello world]=x); echo \"same=$?\"\n" +
			"echo \"after=$?\"\n" +
			"declare -p d\n" +
			"declare -a e=(base)\n" +
			"e+=([hello world]=y); echo \"app=$?\"\n" +
			"declare -p e\n",
		needs: `declare -a e=([0]="base")`,
	},
	{
		// The associative half needs no stderr at all: the text between
		// the brackets is the key, with quotes removed, metacharacters
		// kept, and expansions run — including one whose output holds
		// the bracket the scan is looking for.
		name: "an associative key is the subscript's text",
		body: "declare -A m\n" +
			"m[hello world]=flip\n" +
			"echo \"read=[${m[hello world]}]\"\n" +
			"m['a b']=q; m[c\\ d]=r; m[e \"f\"]=s\n" +
			"echo \"quoted=[${m[a b]}${m[c d]}${m[e f]}]\"\n" +
			"m[$(echo \"a]b\")]=t\n" +
			"echo \"nested=[${m['a]b']}]\"\n" +
			"m[a;b]=1; m[a{b]=2; m[a}b]=3; m[a(b]=4; m[a)b]=5\n" +
			"echo \"meta=[${m[a;b]}${m[a{b]}${m[a}b]}${m[a(b]}${m[a)b]}]\"\n",
		needs: "read=[flip]",
	},
	{
		// A key whose text *also* reads as arithmetic (#626). Both
		// directions are asserted, because the write and the read were
		// wrong in the same direction: `m[x[1]]=q` stored under the
		// empty key and reading it back agreed, so a test of the value
		// alone passed while the array held the wrong thing. One key per
		// array, since `declare -p` lists an associative array in bash's
		// hash order and only a single-key listing is comparable.
		name: "an arithmetic-shaped key is stored under its text",
		body: "declare -A a; a[a-b]=1; declare -p a\n" +
			"declare -A b; b[-1]=2; declare -p b\n" +
			"declare -A c; c[1+2]=3; declare -p c\n" +
			"declare -A d; d[(1)]=4; declare -p d\n" +
			"declare -A e; e[x[1]]=5; declare -p e; echo \"keys=[${!e[@]}]\"\n" +
			"echo \"read=[${a[a-b]}][${b[-1]}][${c[1+2]}][${d[(1)]}][${e[x[1]]}]\"\n" +
			"echo \"tail=$?\"\n",
		needs: `declare -A e=(["x[1]"]="5" )`,
	},
	{
		// The read crashed on an interface conversion, which the panic
		// guard turns into an ordinary diagnostic — so surviving proves
		// nothing and the output is what is asserted. The statements
		// after each read are there to catch a line or a file that was
		// lost instead.
		name: "reading an arithmetic-shaped key answers empty",
		body: "declare -A m\n" +
			"echo \"binary=[${m[a-b]}]\"\n" +
			"echo \"unary=[${m[-1]}]\"\n" +
			"echo \"paren=[${m[(1)]}]\"\n" +
			"echo \"len=${#m[a-b]}\"\n" +
			"echo \"default=[${m[a-b]:-fallback}]\"\n" +
			"echo \"assign=[${m[a-b]=set}]\"\n" +
			"declare -p m\n" +
			"echo \"tail=$?\"\n",
		needs: "default=[fallback]",
	},
	{
		// The same characters in an indexed array are the arithmetic
		// they look like: a subtraction, a count from the end, and a
		// reference to another array's element.
		name: "an indexed subscript reads the same text arithmetically",
		body: "declare -a q=(z0 z1 z2 z3)\n" +
			"q[-1]=last; declare -p q\n" +
			"q[a-b]=zero; declare -p q\n" +
			"q[x[1]]=alsozero; declare -p q\n" +
			"echo \"read=[${q[-1]}][${q[1+2]}]\"\n",
		needs: `[3]="last"`,
	},
	{
		// A nameref and an indirection reach a subscript from a
		// *string*, so each one re-reads it — as a word, not as
		// arithmetic, or the reference names a different key than the
		// one written.
		name: "a reference to an arithmetic-shaped element",
		body: "declare -A m\n" +
			"declare -n ref=m[a-b]\n" +
			"ref=viaref\n" +
			"declare -p m\n" +
			"n=m[a-b]\n" +
			"echo \"indirect=[${!n}]\"\n" +
			"unset \"m[a-b]\"; declare -p m\n",
		needs: `declare -A m=([a-b]="viaref" )`,
	},
	{
		// Spacing is part of a key and is trimmed by the arithmetic
		// reader, so ` a - b ` and `a-b` are two keys while ` 1 ` and
		// `1` are one index.
		name: "a key keeps its spacing",
		body: "declare -A m\n" +
			"m[ a - b ]=spaced\n" +
			"m[a-b]=tight\n" +
			"echo \"spaced=[${m[ a - b ]}] tight=[${m[a-b]}]\"\n" +
			"declare -A s; s[ k ]=v; declare -p s\n" +
			"a=(x y z); echo \"index=[${a[ 1 ]}]\"\n",
		needs: `declare -A s=([" k "]="v" )`,
	},
	{
		// The bracket that completes a compound element's `]=` is the
		// matching one, so the nested pair inside the key is counted
		// rather than stopping the scan.
		name: "a compound element's subscript may hold brackets",
		body: "declare -A m=([x[1]]=three)\n" +
			"declare -p m\n" +
			"echo \"read=[${m[x[1]]}]\"\n" +
			"declare -A n=([a-b]=one); declare -p n\n",
		needs: `declare -A m=(["x[1]"]="three" )`,
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
