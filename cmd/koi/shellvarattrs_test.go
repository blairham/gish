//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BASH_VERSINFO is readonly, which is a claim about the shell around the
// interpreter rather than about a runner: the array is declared by
// internal/repl's identity source, so nothing in internal/shell/interp
// can see it and the case has to run a real koi (#691).
//
// It matters because BASH_VERSINFO is a **feature probe** — fzf picks its
// Ctrl-T implementation on `BASH_VERSINFO[0] < 4` — and a probe a script
// can silently overwrite is one whose answer nothing can rely on. koi
// listed it as `declare -a` and took a write at 0.
//
// The values themselves are #120's decided divergence (koi claims bash's
// interface, not bash's identity), so this compares the *attribute* and
// the statuses rather than the array's contents.
func TestBashVersinfoIsReadonly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	const script = "declare -p BASH_VERSINFO >/dev/null; echo listed=$?\n" +
		"( BASH_VERSINFO[0]=9 ) 2>/dev/null; echo elem=$?\n" +
		"( BASH_VERSINFO=(1 2) ) 2>/dev/null; echo whole=$?\n" +
		"unset BASH_VERSINFO 2>/dev/null; echo unset=$?\n" +
		"echo probe=${BASH_VERSINFO[0]}\n"

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.sh"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	bashOut, _ := runInDir(t, dir, bash, "./s.sh")
	koiOut, _ := runInDir(t, dir, koi, "./s.sh")

	// The oracle has to have refused, or every assertion below would pass
	// against a shell that never had the variable at all.
	if !strings.Contains(bashOut, "elem=1") {
		t.Fatalf("the oracle took the write, so this case cannot detect a missing refusal: %q", bashOut)
	}
	for _, want := range []string{"listed=0", "elem=1", "whole=1", "unset=1"} {
		if !strings.Contains(koiOut, want) {
			t.Errorf("koi did not answer %q: %q", want, koiOut)
		}
		if !strings.Contains(bashOut, want) {
			t.Errorf("bash did not answer %q, so the expectation is wrong: %q", want, bashOut)
		}
	}
	// The refusals must not have cost the probe its answer.
	if !strings.Contains(koiOut, "probe=5") {
		t.Errorf("the probe stopped answering: %q", koiOut)
	}

	// The listing carries the r, which is the line array.tests compares.
	attrs, _ := runInDir(t, dir, koi, "-c", "declare -p BASH_VERSINFO")
	if !strings.HasPrefix(attrs, "declare -ar BASH_VERSINFO=(") {
		t.Errorf("declare -p BASH_VERSINFO = %q, want a `declare -ar` line", attrs)
	}
}
