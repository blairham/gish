//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// An alias in the rc must work at the prompt (#53).
//
// This is the migration case: a switcher's rc is mostly aliases, and
// before this the `alias` builtin recorded the definition while the
// shell never expanded it — so defining one looked like it worked and
// using it reported "command not found".
//
// Driven through a real pty because that is the only path that enables
// expansion; the -c and script paths deliberately leave aliases off, and
// a unit test of those would have "passed" while the shell was broken.
func TestAliasFromRCWorksInteractively(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}
	dir := t.TempDir()
	rc := filepath.Join(dir, "gishrc")
	if err := os.WriteFile(rc, []byte("alias greet='printf res%s\\n'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := startPTY(t, ptyOptions{Dir: dir, Env: []string{"GISH_RC=" + rc}})
	s.waitForPrompt()
	s.send("greet ALIASOK\r")
	s.waitFor("resALIASOK")
}
