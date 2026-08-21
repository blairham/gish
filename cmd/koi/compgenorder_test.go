//go:build unix

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// compgen's candidate *order*, which is part of the answer (#613).
//
// #269 recorded koi's sort-and-collapse as harmless, and for a listing on
// stdout it nearly is. `compgen -V` (#556) is what makes it addressable:
// the candidates go into an array, `${arr[0]}` is the first one, and a
// completion function offering its best guess offers that element. bash's
// own complete.tests reads it. The second half is that a wordlist's order
// is the caller's answer — `compgen -W "$opts"` is the commonest line in
// the bash-completion corpus and `$opts` is written most-likely-first.
//
// Every case here compares against bash **in order**, which is what the
// existing cases deliberately do not do: compgen_test.go sorts both sides
// because it is asking about the *set*. The two are different questions
// and both are worth asking, so this file is order and that one is
// membership.

// TestCompgenKeepsGenerationOrder is the differential half: same script,
// both shells, output compared as written.
func TestCompgenKeepsGenerationOrder(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	for _, tc := range []struct {
		name, script string
		// needsCoproc marks a case whose expected output contains bash's
		// twenty-second keyword. macOS ships bash 3.2, which predates it,
		// so there the *set* differs and an order comparison would be
		// measuring #269's list rather than #613's order.
		needsCoproc bool
	}{
		// The wordlist, which is the case that matters most: this is what
		// a completion function writes, and koi answered `a b c`.
		{name: "a wordlist keeps its order", script: `compgen -W "c a b"`},
		{name: "a longer wordlist", script: `compgen -W "zebra apple mango apple"`},

		// De-duplication is the smaller half and is also a divergence:
		// three candidates in bash, two in koi.
		{name: "duplicates are candidates", script: `compgen -W "a a b"`},
		{name: "duplicates with a prefix", script: `compgen -W "aa ab aa" a`},

		// A table koi owns, so the order is koi's to get right: bash's
		// reserved words are in its own table order, where `in` follows
		// `until do done` and the punctuation trails.
		{name: "the keyword table", script: `compgen -k`, needsCoproc: true},

		// Which generator runs first, which bash fixes internally rather
		// than reading off the argv: the actions come first and the
		// wordlist last, both ways round.
		{name: "actions before the wordlist", script: `compgen -W "zz aa" -k`, needsCoproc: true},
		{name: "and the same with the options swapped", script: `compgen -k -W "zz aa"`, needsCoproc: true},

		// The shaping options apply per candidate and must not reorder.
		{name: "prefix and suffix keep the order", script: `compgen -P "<" -S ">" -W "c a b"`},
		{name: "the filter keeps the order", script: `compgen -X "b*" -W "c a b bb"`},

		// `-o nosort` is bash's way of saying "do not sort", and since
		// bash never sorted, it changes nothing — which is the assertion
		// that koi's not-sorting is not secretly conditional.
		{name: "nosort changes nothing", script: `compgen -o nosort -W "c a b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.needsCoproc && !oracleHas(t, bash, featCoproc) {
				t.Skipf("bash here has no %s (%s), so its keyword set differs from koi's",
					featCoproc, bashVersion(t, bash))
			}
			dir := t.TempDir()
			got, gotStatus := shellRows(t, koi, dir, tc.script)
			want, wantStatus := shellRows(t, bash, dir, tc.script)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s:\n  koi:  %q\n  bash: %q", tc.script, got, want)
			}
			if gotStatus != wantStatus {
				t.Errorf("%s: koi status %d, bash %d", tc.script, gotStatus, wantStatus)
			}
		})
	}
}

// The array `-V` fills is the listing, in the listing's order, and that
// is the reason this issue is not cosmetic: `${arr[0]}` is a value a
// completion function acts on.
func TestCompgenVArrayIsInGenerationOrder(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	if !oracleHas(t, bash, featCompgenV) {
		t.Skipf("bash here has no compgen -V (%s) — no oracle for this case", bashVersion(t, bash))
	}

	for _, script := range []string{
		// The element bash's own complete.tests reads.
		`compgen -V arr -W "cherry apple banana" >/dev/null; echo "${arr[0]}"`,
		`compgen -V arr -W "cherry apple banana" >/dev/null; declare -p arr`,
		// Duplicates reach the array too, so its length is the number of
		// candidates generated rather than the number of distinct ones.
		`compgen -V arr -W "a a b" >/dev/null; echo "${#arr[@]}"`,
	} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			gotOut, gotCode := runShell(t, koi, script)
			wantOut, wantCode := runShell(t, bash, script)
			if gotOut != wantOut {
				t.Errorf("%s:\n  koi:  %q\n  bash: %q", script, gotOut, wantOut)
			}
			if gotCode != wantCode {
				t.Errorf("%s: koi status %d, bash %d", script, gotCode, wantCode)
			}
		})
	}
}

// The listings bash sorts have to stay sorted, which is the other half of
// deleting a global sort: a change that made every generator answer in
// map-iteration order would pass every case above and answer a different
// list on each run. These are koi's own listings — the builtin set is
// koi's 38 to bash's 61 (#269) — so the claim is sortedness rather than
// agreement with bash, and each one is asserted non-empty, because a
// listing of nothing is sorted.
func TestCompgenSortedListingsStaySorted(t *testing.T) {
	t.Parallel()
	koi := buildKoi(t)
	dir := t.TempDir()

	for _, tc := range []struct{ name, script string }{
		{"builtins", `compgen -b`},
		{"enabled builtins", `compgen -A enabled`},
		{"set -o names", `compgen -A setopt`},
		{"shopt names", `compgen -A shopt`},
		{"help topics", `compgen -A helptopic`},
		{"variables", `zz=1 aa=2 mm=3; compgen -v`},
		{"exports", `export zz=1 aa=2 mm=3; compgen -e`},
		{"aliases", `alias zz=1 aa=2 mm=3; compgen -a`},
		{"functions", `zf(){ :; }; af(){ :; }; mf(){ :; }; compgen -A function`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := shellRows(t, koi, dir, tc.script)
			if len(got) == 0 {
				t.Fatalf("%s listed nothing, so sortedness proves nothing", tc.script)
			}
			if !slices.IsSorted(got) {
				t.Errorf("%s is not sorted: %q", tc.script, got)
			}
		})
	}
}

// `-f` and `-d` are the one deliberate divergence, and it is recorded
// here rather than left to be rediscovered. bash's listing is a raw
// readdir — on a fixture created z, a, m it answers in creation order —
// and Go's os.ReadDir sorts, which is a property of the C library rather
// than of the shell. A sorted listing is the more useful of the two, so
// what is asserted is that koi's is sorted and that the two shells
// generate the same *set*.
//
// The fixture is built in a deliberately unsorted order so the two
// answers can differ at all; if this machine's readdir happens to be
// sorted the case still holds, and says so.
func TestCompgenPathListingIsSortedNotReaddir(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	dir := t.TempDir()
	for _, name := range []string{"zdir", "adir", "mdir"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"zfile", "afile", "mfile"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, script := range []string{"compgen -f", "compgen -d"} {
		got, _ := shellRows(t, koi, dir, script)
		want, _ := shellRows(t, bash, dir, script)
		if len(got) == 0 {
			t.Fatalf("%s listed nothing", script)
		}
		if !slices.IsSorted(got) {
			t.Errorf("%s is not sorted: %q", script, got)
		}
		gotSet, wantSet := slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))
		if strings.Join(gotSet, "\n") != strings.Join(wantSet, "\n") {
			t.Errorf("%s: koi and bash list different names:\n  koi:  %q\n  bash: %q", script, gotSet, wantSet)
		}
		if slices.IsSorted(want) {
			t.Logf("%s: this filesystem's readdir is already sorted, so bash agrees here by accident: %q", script, want)
		}
	}
}
