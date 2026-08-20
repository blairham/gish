package repl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/interp"
)

// The preload is gated on a session that records ambiently (#432).
//
// An interactive session's history is the store shared live across
// sessions (#40), and `set -o history` in an inherited rc would
// otherwise replace what the user sees with a file's contents. The gate
// is the whole reason the incremental forms could be implemented at all
// without disturbing that posture, so it gets its own test rather than
// riding on the script-path cases.
func TestHistoryPreloadOnlyInAmbientSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hf")
	if err := os.WriteFile(path, []byte("from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// An explicit environment, never the process's: interp.Env(nil)
	// snapshots os.Environ() at construction, so a runner built before a
	// t.Setenv still carries the developer's own $HISTFILE — and this
	// test would then read their real shell history. It did exactly that
	// once, which is the whole argument for the ListEnviron here.
	runner, err := interp.New(
		interp.Dir(dir),
		interp.Env(expand.ListEnviron("HISTFILE="+path)),
	)
	if err != nil {
		t.Fatal(err)
	}
	setSessionRunner(runner)

	reset := func(ambient bool) {
		histMu.Lock()
		histList, histMutated = nil, false
		histBase, histAppendPos, histFileLines = 0, 0, 0
		histPreloaded, histAmbientSession = false, ambient
		histMu.Unlock()
	}
	t.Cleanup(func() { reset(false) })

	// Not ambient: the file is not read, and nothing claims the list.
	reset(false)
	historyPreload(interp.HandlerContext{Dir: dir})
	histMu.Lock()
	mutated, entries := histMutated, len(histList)
	histMu.Unlock()
	if mutated || entries != 0 {
		t.Fatalf("an interactive session loaded the history file: mutated=%v entries=%d", mutated, entries)
	}

	// Ambient: loaded once, and a second enable does not reload.
	reset(true)
	historyPreload(interp.HandlerContext{Dir: dir})
	historyPreload(interp.HandlerContext{Dir: dir})
	histMu.Lock()
	got, pos, lines := append([]string(nil), histList...), histAppendPos, histFileLines
	histMu.Unlock()
	if len(got) != 1 || got[0] != "from-the-file" {
		t.Fatalf("list = %q, want the file's one entry loaded exactly once", got)
	}
	if pos != 1 || lines != 1 {
		t.Fatalf("appendPos=%d fileLines=%d, want 1 and 1 — the preload marks its entries written", pos, lines)
	}
}
