//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// What an aborting error costs a sourced file (#585).
//
// #469's category means "abandon this input unit and go back to reading",
// and in a file the unit is the line (#450). `Runner.runReading` treated
// it like `exit` instead and stopped reading the file, so a sourced
// library lost every line after a bad substitution — and the flag
// escaped with the source's status, so the *caller* lost the rest of its
// own line too.
//
// Both halves are here because they fail separately, and the second one
// is the one nobody would think to check: the caller's line continues.
//
// Differential, and run as script files rather than through compat.Run's
// `-c`, because the line is the unit under test and a command string is
// only ever one unit.
var sourceAbortCases = []struct {
	name  string
	main  string
	lib   string
	needs string // a string the oracle must print, so the case cannot pass vacuously
}{
	{
		// The library carries on at its next line.
		name:  "the sourced file keeps reading",
		main:  "echo main-one\n. ./lib.sh\necho main-three\n",
		lib:   "echo lib-one\necho ${${bad}}\necho lib-three\n",
		needs: "lib-three",
	},
	{
		// And the caller's own line is not collateral: the statement
		// after the `.` still runs, and reads status 1.
		name:  "the caller's line survives it",
		main:  "echo main-one; . ./lib.sh; echo same-line=$?\necho next-line\n",
		lib:   "echo lib-one\necho ${${bad}}\n",
		needs: "same-line=1",
	},
	{
		// The rest of the *library's* line is still abandoned, which is
		// the half that must not regress into "ignore the error".
		name:  "the rest of the library's line is still lost",
		main:  ". ./lib.sh\necho main-two\n",
		lib:   "echo lib-one; echo ${${bad}}; echo lib-same-line\necho lib-two\n",
		needs: "lib-two",
	},
	{
		// `exit` and `return` still end the file, so the fix is about
		// the aborting category and not about reading past everything.
		name:  "exit still ends the sourced file",
		main:  ". ./lib.sh\necho unreached\n",
		lib:   "echo lib-one\nexit 3\necho lib-three\n",
		needs: "lib-one",
	},
	{
		name:  "return still ends the sourced file",
		main:  ". ./lib.sh\necho \"after=$?\"\n",
		lib:   "echo lib-one\nreturn 4\necho lib-three\n",
		needs: "after=4",
	},
}

func TestAbortingErrorInASourcedFileCostsTheLine(t *testing.T) {
	if testing.Short() {
		t.Skip("differential source semantics skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	for _, tc := range sourceAbortCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, body := range map[string]string{"main.sh": tc.main, "lib.sh": tc.lib} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			bashOut, bashCode := runInDir(t, dir, bash, "./main.sh")
			koiOut, koiCode := runInDir(t, dir, koi, "./main.sh")
			if bashOut != koiOut || bashCode != koiCode {
				t.Errorf("%s differs from bash\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
					tc.name, bashOut, bashCode, koiOut, koiCode)
			}
			if !containsLine(bashOut, tc.needs) {
				t.Errorf("%s: the oracle never printed %q, so the case proves nothing: %q",
					tc.name, tc.needs, bashOut)
			}
			// The negative half of case three needs its own assertion:
			// what must be absent, in both shells.
			if tc.name == "the rest of the library's line is still lost" {
				for shell, out := range map[string]string{"bash": bashOut, "koi": koiOut} {
					if containsLine(out, "lib-same-line") {
						t.Errorf("%s ran the rest of the aborted line: %q", shell, out)
					}
				}
			}
		})
	}
}

func containsLine(out, want string) bool {
	for line := range splitLines(out) {
		if line == want {
			return true
		}
	}
	return false
}

// splitLines yields out's lines without the trailing empty one, so an
// exact-match check does not have to think about the final newline.
func splitLines(out string) func(func(string) bool) {
	return func(yield func(string) bool) {
		start := 0
		for i := range len(out) {
			if out[i] != '\n' {
				continue
			}
			if !yield(out[start:i]) {
				return
			}
			start = i + 1
		}
		if start < len(out) {
			yield(out[start:])
		}
	}
}
