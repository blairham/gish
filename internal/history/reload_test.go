package history_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/blairham/gish/internal/history"
)

// TestCrossSessionReload is #40: a second session's appends become
// visible to the first without reopening.
func TestCrossSessionReload(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "h.jsonl")
	s1, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s1.SetSession("one")

	s2, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.SetSession("two")

	if _, err := s2.Append(history.Entry{Command: "from-two", SessionID: "two"}); err != nil {
		t.Fatal(err)
	}

	// s1 sees s2's command on lookup, live.
	if got, ok := s1.Match("from", 0); !ok || got != "from-two" {
		t.Fatalf("Match = %q,%v", got, ok)
	}
}

// TestReloadSkipsOwnEntries: our own appends are already in memory; a
// reload must not duplicate them.
func TestReloadSkipsOwnEntries(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "h.jsonl")
	s, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetSession("me")

	if _, err := s.Append(history.Entry{Command: "alpha", SessionID: "me"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(history.Entry{Command: "beta", SessionID: "me"}); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Match("", 1); !ok || got != "alpha" {
		t.Fatalf("Match(1) = %q,%v — duplicate or missing", got, ok)
	}
	if _, ok := s.Match("", 2); ok {
		t.Fatal("own entries duplicated by reload")
	}
}

// TestReloadToleratesPartialLine: a concurrent writer mid-line must not
// corrupt ingestion; the completed line arrives on the next reload.
func TestReloadToleratesPartialLine(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "h.jsonl")
	s, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetSession("me")

	// O_CREATE because opening a store no longer creates the file (#163):
	// a shell that has run nothing writes nothing. Here it stands in for
	// the other session, which is who creates it in practice.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Half a line: not consumed.
	if _, err := f.WriteString(`{"command":"parti`); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Match("parti", 0); ok {
		t.Fatal("partial line ingested")
	}
	// Complete it: consumed now.
	if _, err := f.WriteString(`al","session_id":"other"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Match("parti", 0); !ok || got != "partial" {
		t.Fatalf("completed line = %q,%v", got, ok)
	}
}
