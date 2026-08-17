//go:build unix

package compat_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/gish/internal/compat"
)

// The fzf case has two ways in, and a developer laptop only ever
// exercises one of them.
//
// `fzf --bash` arrived in 0.48. Ubuntu 24.04 ships 0.44.1, whose
// integration is the packaged key-bindings file — and CI is where that
// surfaced, as "unknown option: --bash" and an unbound Ctrl-T. This
// stands up an fzf that predates the flag so the fallback is covered
// wherever the tests run, rather than only on the runner that happens
// to have the old one.
func TestFzfIntegrationFallsBackToThePackagedFile(t *testing.T) {
	gishBin := buildGish(t)

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// An fzf that refuses --bash, exactly as 0.44 does.
	fake := "#!/bin/sh\ncase \"$1\" in --bash) echo 'unknown option: --bash' >&2; exit 2;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "fzf"), []byte(fake), 0o755); err != nil { //nolint:gosec // a fixture on PATH
		t.Fatal(err)
	}
	// The key-bindings file Debian and Ubuntu package, reduced to the
	// branch that matters: bash 4+ gets `bind -x`, which is the binding
	// gish implements.
	keys := filepath.Join(dir, "key-bindings.bash")
	body := `if [[ $- =~ i ]]; then
  fzf-file-widget() { echo widget; }
  if (( BASH_VERSINFO[0] < 4 )); then
    bind -m emacs-standard '"\C-t": " \C-b\C-k"'
  else
    bind -m emacs-standard -x '"\C-t": fzf-file-widget'
  fi
fi
`
	if err := os.WriteFile(keys, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var fzfCase compat.EcosystemCase
	for _, c := range compat.EcosystemCorpus {
		if c.Name == "fzf" {
			fzfCase = c
		}
	}
	if fzfCase.Name == "" {
		t.Fatal("the fzf case is gone from the corpus")
	}
	// Redirect the first candidate path at the fixture; the rest of the
	// snippet — including the `--bash` attempt that must fail first — is
	// the published one.
	fzfCase.Init = strings.Replace(fzfCase.Init,
		"/usr/share/doc/fzf/examples/key-bindings.bash", keys, 1)

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := compat.RunEcosystem(context.Background(), gishBin, fzfCase)
	if !r.Present {
		t.Fatal("the fixture fzf was not found on PATH")
	}
	if !r.Pass {
		t.Errorf("the packaged-file fallback did not bind Ctrl-T: %s\n%s", r.Reason, r.Output)
	}
}
