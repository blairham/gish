//go:build unix

package main

import (
	"strconv"
	"strings"
	"testing"
)

// ulimit, differentially (#250).
//
// These run koi as a process rather than through interp's own table, and
// that is not a stylistic choice: `ulimit -n 512` calls setrlimit on the
// calling process, so a case in the in-process table would change the
// test binary's own limits and leak into every test that ran after it.
// A limit is real user state, and the rule about not touching it applies
// to the process as much as to the home directory.
//
// bash is the oracle throughout: the values are whatever this machine
// happens to allow, so nothing here can be written down in advance.

// bashLimitLetters returns the option letters bash itself reports, read
// out of its own `ulimit -a`. Asking bash rather than listing them keeps
// the test honest across platforms — linux reports six resources darwin
// has never heard of, and a hardcoded list would quietly stop covering
// them.
func bashLimitLetters(t *testing.T, bash, dir string) []string {
	t.Helper()
	lines, _ := shellLines(t, bash, dir, "ulimit -a")
	var out []string
	for _, line := range lines {
		open := strings.LastIndex(line, "(")
		close := strings.Index(line[max(open, 0):], ")")
		if open < 0 || close < 0 {
			t.Fatalf("cannot read an option letter out of %q", line)
		}
		field := line[open+1 : open+close]
		out = append(out, field[len(field)-1:])
	}
	if len(out) == 0 {
		t.Fatal("bash reported no limits at all")
	}
	return out
}

// nofileIsRuntimeOwned explains the one letter that cannot be compared.
//
// The Go runtime rewrites RLIMIT_NOFILE for its own process before main
// runs — raising the soft limit to near the hard one on linux, and on
// darwin clamping it to the kernel's real per-process ceiling, which is
// *lower* than the nominal limit a login shell carries. os/exec then
// restores the original for every child, so koi's children get exactly
// what bash's do; it is only koi's own view of the number that differs.
// The original is saved in an unexported runtime variable with no
// accessor, so koi cannot report it. What matters to a script — that
// setting the limit reaches the programs it then runs — is asserted
// directly by TestUlimitSetReachesChildren.
const nofileIsRuntimeOwned = "n"

func TestUlimitMatchesBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	for _, letter := range bashLimitLetters(t, bash, dir) {
		if letter == nofileIsRuntimeOwned {
			continue
		}
		for _, which := range []string{"-", "-S", "-H"} {
			script := "ulimit " + which + letter
			t.Run(script, func(t *testing.T) {
				t.Parallel()
				got, gotStatus := shellLines(t, koi, dir, script)
				want, wantStatus := shellLines(t, bash, dir, script)
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("%s: koi %q, bash %q", script, got, want)
				}
				if gotStatus != wantStatus {
					t.Errorf("%s: koi status %d, bash %d", script, gotStatus, wantStatus)
				}
			})
		}
	}
}

// The labeled form, which pins the descriptions, the units, the option
// order and the padding all at once — `ulimit -a | grep 'open files'` is
// a normal way to read this, so the strings are a contract.
func TestUlimitListingMatchesBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	for _, script := range []string{"ulimit -a", "ulimit -aH", "ulimit -aS"} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			got, _ := shellLines(t, koi, dir, script)
			want, _ := shellLines(t, bash, dir, script)
			if len(got) != len(want) {
				t.Fatalf("%s: koi listed %d rows, bash %d:\nkoi:  %q\nbash: %q", script, len(got), len(want), got, want)
			}
			for i := range want {
				// The open-files row carries the one value the Go runtime
				// owns; its label and column are still compared.
				if strings.Contains(want[i], "-n)") {
					if labelOf(got[i]) != labelOf(want[i]) {
						t.Errorf("%s row %d: koi %q, bash %q", script, i, labelOf(got[i]), labelOf(want[i]))
					}
					continue
				}
				if got[i] != want[i] {
					t.Errorf("%s row %d:\nkoi:  %q\nbash: %q", script, i, got[i], want[i])
				}
			}
		})
	}
}

// labelOf is everything up to and including the option letter, which is
// the part of a row that does not depend on the machine.
func labelOf(row string) string {
	if i := strings.LastIndex(row, ")"); i >= 0 {
		return row[:i+1]
	}
	return row
}

// Setting a limit and reading it back, which is the half a script acts
// on. These letters are chosen because they can be lowered without
// stopping the shell from working.
func TestUlimitSetMatchesBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	for _, script := range []string{
		"ulimit -c 1; ulimit -c",
		"ulimit -f 100; ulimit -f",
		"ulimit -s 1024; ulimit -s",
		"ulimit -c 0; ulimit -c; ulimit -Hc",     // the soft half moves alone
		"ulimit -S -c 1; ulimit -c",              // spelled out
		"ulimit -c unlimited; ulimit -c",         // the word, not a number
		"ulimit -c 1; ulimit -c hard; ulimit -c", // hard as a value
		// Naming neither half moves both, so the ceiling comes down with
		// the limit and cannot be raised again. A shell that moved only
		// the soft half would let this second set succeed, and the
		// difference is permanent rather than cosmetic.
		"ulimit -c 1; ulimit -c 2; ulimit -c",
		"ulimit -S -c 1; ulimit -S -c 2; ulimit -c", // the soft half alone still can
	} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			got, gotStatus := shellLines(t, koi, dir, script)
			want, wantStatus := shellLines(t, bash, dir, script)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s: koi %q, bash %q", script, got, want)
			}
			if gotStatus != wantStatus {
				t.Errorf("%s: koi status %d, bash %d", script, gotStatus, wantStatus)
			}
		})
	}
}

// The contract that makes ulimit worth having: a limit set in the shell
// applies to the programs the shell then runs. This is the assertion
// that stands in for comparing `ulimit -n` directly, and it is the one
// that would fail if koi set the limit through a path that bypassed the
// runtime's bookkeeping — the child would silently get the old value.
func TestUlimitSetReachesChildren(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	const want = "512"
	script := "ulimit -n " + want + "; " + bash + " -c 'ulimit -n'"

	for _, shell := range []struct{ name, bin string }{{"koi", koi}, {"bash", bash}} {
		got, status := shellLines(t, shell.bin, dir, script)
		if status != 0 || strings.Join(got, "") != want {
			t.Errorf("under %s a child saw %q (status %d), want %q", shell.name, got, status, want)
		}
	}
}

// The option grammar, which is not the ordinary one: a resource letter
// takes an optional argument, so -nH is "-n with the limit H" and not
// "-n and -H". Statuses only for the failures — koi's diagnostics carry
// its own prefixes by design (#120), so the text cannot be compared.
func TestUlimitOptionGrammarMatchesBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	compare := func(t *testing.T, script string, output bool) {
		t.Helper()
		got, gotStatus := shellLines(t, koi, dir, script)
		want, wantStatus := shellLines(t, bash, dir, script)
		if gotStatus != wantStatus {
			t.Errorf("%s: koi status %d, bash %d", script, gotStatus, wantStatus)
		}
		if output && strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: koi %q, bash %q", script, got, want)
		}
	}

	t.Run("shapes that answer", func(t *testing.T) {
		t.Parallel()
		for _, script := range []string{
			"ulimit",         // bare is -f
			"ulimit -c -s",   // more than one resource takes the labeled form
			"ulimit -s -c",   // and keeps the order asked
			"ulimit -HS -c",  // asking answers soft whenever S is there at all,
			"ulimit -SH -c",  // in either order — it is not last-one-wins
			"ulimit -c 1 -s", // a value binds to the letter before it
			"ulimit -c 1 2",  // and a spare word is ignored
			"ulimit -a -c",   // -a wins over a resource beside it
		} {
			t.Run(script, func(t *testing.T) {
				t.Parallel()
				compare(t, script, !strings.Contains(script, "-a"))
			})
		}
	})

	t.Run("shapes that fail", func(t *testing.T) {
		t.Parallel()
		for _, script := range []string{
			"ulimit -nH", // H read as the limit, not as an option
			"ulimit -sc", // likewise c
			"ulimit -c zz",
			"ulimit -z",
		} {
			t.Run(script, func(t *testing.T) {
				t.Parallel()
				compare(t, script, false)
				if _, status := shellLines(t, koi, dir, script); status == 0 {
					t.Errorf("%s: koi exited 0", script)
				}
			})
		}
	})
}

// The units, which are the part that produces a plausible wrong answer
// rather than an obvious one: a stack reported in bytes rather than
// kbytes is out by a factor of 1024 and still looks like a limit.
func TestUlimitReportsInBashsUnits(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	// Setting in the reported units must round-trip through both shells
	// identically, and the kernel holds bytes, so a wrong factor shows up
	// as a different number coming back.
	for _, tc := range []struct{ letter, set string }{
		{"s", "1024"}, // kbytes
		{"f", "100"},  // 512-byte blocks
		{"c", "2"},    // 512-byte blocks
	} {
		script := "ulimit -" + tc.letter + " " + tc.set + "; ulimit -" + tc.letter
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			got, _ := shellLines(t, koi, dir, script)
			want, _ := shellLines(t, bash, dir, script)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s: koi %q, bash %q", script, got, want)
			}
			if n, err := strconv.Atoi(strings.Join(got, "")); err != nil || strconv.Itoa(n) != tc.set {
				t.Errorf("%s: koi answered %q, want the value it was given back", script, got)
			}
		})
	}

	// Reading the value back through the same shell cannot see a wrong
	// factor: setting 2 blocks as 2 bytes and then dividing by 1 again
	// answers 2 either way. The kernel is holding the difference, so the
	// check has to come from outside — a child bash divides the raw
	// number by its own idea of the unit, and only agrees if koi's
	// matched.
	for _, tc := range []struct{ letter, set string }{
		{"s", "2048"},
		{"f", "64"},
		{"c", "2"},
	} {
		script := "ulimit -" + tc.letter + " " + tc.set + "; " + bash + " -c 'ulimit -" + tc.letter + "'"
		t.Run("through a child: "+script, func(t *testing.T) {
			t.Parallel()
			got, _ := shellLines(t, koi, dir, script)
			want, _ := shellLines(t, bash, dir, script)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s:\nkoi:  %q\nbash: %q", script, got, want)
			}
		})
	}
}
