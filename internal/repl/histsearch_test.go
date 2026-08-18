package repl

import (
	"testing"

	"github.com/blairham/koi-shell/internal/history"
)

// The merge rule that matters: the local store is authoritative, so a
// backend's copy of a command this machine also ran must never replace
// the local metadata. A backend entry carries another machine's cwd and
// another machine's clock, and showing that would be a quiet lie about
// where the command ran.
func TestMergeHistoryPrefersLocalMetadata(t *testing.T) {
	local := []history.Entry{
		{Command: "git status", Cwd: "/home/me/project", StartedUnixMs: 1000, ExitCode: 0},
	}
	backend := []history.Entry{
		{Command: "git status", Cwd: "/srv/other-machine", StartedUnixMs: 9999, ExitCode: 1},
	}

	got := mergeHistory(local, backend, 100)
	if len(got) != 1 {
		t.Fatalf("duplicate command produced %d rows: %+v", len(got), got)
	}
	if got[0].Cwd != "/home/me/project" || got[0].ExitCode != 0 || got[0].StartedUnixMs != 1000 {
		t.Errorf("backend metadata overwrote local: %+v", got[0])
	}
}

// The whole point of a backend: commands this machine never ran.
func TestMergeHistoryAddsRemoteCommands(t *testing.T) {
	local := []history.Entry{{Command: "ls", StartedUnixMs: 10}}
	backend := []history.Entry{
		{Command: "kubectl get pods", StartedUnixMs: 30},
		{Command: "terraform plan", StartedUnixMs: 20},
	}

	got := mergeHistory(local, backend, 100)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(got), got)
	}
	if got[0].Command != "ls" {
		t.Errorf("local entry did not come first: %+v", got)
	}
	// Backend rows follow, newest first.
	if got[1].Command != "kubectl get pods" || got[2].Command != "terraform plan" {
		t.Errorf("backend rows not newest-first: %+v", got[1:])
	}
}

func TestMergeHistoryDedupesWithinEachSource(t *testing.T) {
	local := []history.Entry{{Command: "ls"}, {Command: "ls"}}
	backend := []history.Entry{{Command: "top"}, {Command: "top"}}

	got := mergeHistory(local, backend, 100)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (one per distinct command): %+v", len(got), got)
	}
}

// The cap bounds what the picker loads. Local entries are never dropped
// to make room for backend ones — the authoritative source keeps its
// seats.
func TestMergeHistoryCapsWithoutEvictingLocal(t *testing.T) {
	local := []history.Entry{{Command: "a"}, {Command: "b"}}
	backend := []history.Entry{{Command: "c"}, {Command: "d"}, {Command: "e"}}

	got := mergeHistory(local, backend, 3)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want the cap of 3: %+v", len(got), got)
	}
	if got[0].Command != "a" || got[1].Command != "b" {
		t.Errorf("local entries were evicted by the cap: %+v", got)
	}
}

// A cap smaller than the local set still returns every local entry:
// truncating the authoritative source to make room for nothing would be
// the worst of both.
func TestMergeHistoryNeverTruncatesLocal(t *testing.T) {
	local := []history.Entry{{Command: "a"}, {Command: "b"}, {Command: "c"}}
	got := mergeHistory(local, nil, 2)
	if len(got) != 3 {
		t.Errorf("local truncated to %d; local is authoritative: %+v", len(got), got)
	}
}

func TestMergeHistoryWithNoBackend(t *testing.T) {
	local := []history.Entry{{Command: "ls"}, {Command: "cd /tmp"}}
	got := mergeHistory(local, nil, 100)
	if len(got) != 2 {
		t.Fatalf("no-backend merge changed the local list: %+v", got)
	}
}

// A nil host is the zero-plugin case, which must be the common, fast,
// silent path — not a special case anyone has to remember.
func TestSearchBackendsWithNoHostReturnsNothing(t *testing.T) {
	if got := searchBackends(t.Context(), nil, "query", "/tmp", 10, false); got != nil {
		t.Errorf("nil host returned %+v, want nil", got)
	}
}

func TestCommandSetMarksRemoteRows(t *testing.T) {
	local := []history.Entry{{Command: "ls"}}
	set := commandSet(local)

	if _, ok := set["ls"]; !ok {
		t.Error("local command missing from the set")
	}
	if _, ok := set["kubectl get pods"]; ok {
		t.Error("a command the local store never had is in the set")
	}
}
