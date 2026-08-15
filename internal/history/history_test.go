package history_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/blairham/gish/internal/history"
)

func open(t *testing.T, path string) *history.Store {
	t.Helper()
	s, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func appendCmd(t *testing.T, s *history.Store, cmd string) {
	t.Helper()
	if _, err := s.Append(history.Entry{Command: cmd, Cwd: "/tmp", SessionID: "test"}); err != nil {
		t.Fatal(err)
	}
}

func TestAppendAndReload(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gish", "history.jsonl")
	s := open(t, path)
	appendCmd(t, s, "echo one")
	appendCmd(t, s, "echo two")
	s.Close()

	s2 := open(t, path)
	if got, ok := s2.Match("", 0); !ok || got != "echo two" {
		t.Errorf("Match(0) = %q,%v", got, ok)
	}
	if got, ok := s2.Match("", 1); !ok || got != "echo one" {
		t.Errorf("Match(1) = %q,%v", got, ok)
	}
}

func TestCorruptLinesSkipped(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "history.jsonl")
	content := `{"command":"good one"}
NOT JSON AT ALL
{"command":""}
{"command":"good two"}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := open(t, path)
	if got, ok := s.Match("", 0); !ok || got != "good two" {
		t.Errorf("Match(0) = %q,%v", got, ok)
	}
	if got, ok := s.Match("", 1); !ok || got != "good one" {
		t.Errorf("Match(1) = %q,%v", got, ok)
	}
	if _, ok := s.Match("", 2); ok {
		t.Error("corrupt/empty lines were loaded")
	}
}

func TestIgnoreSpaceAndConsecutiveDups(t *testing.T) {
	t.Parallel()

	s := open(t, filepath.Join(t.TempDir(), "h.jsonl"))
	appendCmd(t, s, " secret --token=x")
	appendCmd(t, s, "ls")
	appendCmd(t, s, "ls")
	appendCmd(t, s, "ls")

	if _, ok := s.Match(" secret", 0); ok {
		t.Error("space-prefixed command was recorded")
	}
	if got, ok := s.Match("", 0); !ok || got != "ls" {
		t.Errorf("Match(0) = %q,%v", got, ok)
	}
	if _, ok := s.Match("", 1); ok {
		t.Error("consecutive duplicates were recorded")
	}
}

func TestMatchPrefix(t *testing.T) {
	t.Parallel()

	s := open(t, filepath.Join(t.TempDir(), "h.jsonl"))
	appendCmd(t, s, "git status")
	appendCmd(t, s, "make build")
	appendCmd(t, s, "git push")

	if got, ok := s.Match("git", 0); !ok || got != "git push" {
		t.Errorf("Match(git,0) = %q,%v", got, ok)
	}
	if got, ok := s.Match("git", 1); !ok || got != "git status" {
		t.Errorf("Match(git,1) = %q,%v", got, ok)
	}
	if _, ok := s.Match("git", 2); ok {
		t.Error("Match(git,2) should be exhausted")
	}
	if _, ok := s.Match("xyz", 0); ok {
		t.Error("Match(xyz,0) should find nothing")
	}
}

func TestSearchDedupesToMostRecent(t *testing.T) {
	t.Parallel()

	s := open(t, filepath.Join(t.TempDir(), "h.jsonl"))
	appendCmd(t, s, "make test")
	appendCmd(t, s, "make lint")
	appendCmd(t, s, "make test")

	if got, ok := s.Search("test", 0); !ok || got != "make test" {
		t.Errorf("Search(test,0) = %q,%v", got, ok)
	}
	// The older "make test" must not appear again.
	if got, ok := s.Search("make", 1); !ok || got != "make lint" {
		t.Errorf("Search(make,1) = %q,%v", got, ok)
	}
	if _, ok := s.Search("make", 2); ok {
		t.Error("duplicate command offered twice")
	}
}

func TestDefaultPathUsesXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/custom/data")
	got, err := history.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/custom/data", "gish", "history.jsonl") {
		t.Errorf("DefaultPath() = %q", got)
	}

	// The home fallback: expectations built from the same source
	// DefaultPath uses (UserHomeDir reads USERPROFILE on Windows).
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/u")
	t.Setenv("USERPROFILE", "/home/u")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err = history.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, ".local", "share", "gish", "history.jsonl") {
		t.Errorf("DefaultPath() = %q", got)
	}
}
