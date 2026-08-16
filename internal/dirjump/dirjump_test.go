package dirjump

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVisitQueryFrecency(t *testing.T) {
	t.Parallel()

	s, err := Open(filepath.Join(t.TempDir(), "dirs.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Old favorite: many visits, a week stale. New spot: fresh.
	for range 10 {
		s.Visit("/work/api-server", now.Add(-8*24*time.Hour))
	}
	s.Visit("/tmp/api-scratch", now.Add(-10*time.Minute))

	got := s.Query([]string{"api"}, now)
	if len(got) != 2 {
		t.Fatalf("matches = %+v", got)
	}
	// 1 visit * 4 (hour bucket) > 10 visits * 0.25 (stale bucket).
	if got[0].Dir != "/tmp/api-scratch" {
		t.Errorf("recency lost to raw count: %+v", got)
	}
}

func TestQueryZoxideMatchingRules(t *testing.T) {
	t.Parallel()

	s, err := Open(filepath.Join(t.TempDir(), "dirs.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.Visit("/home/u/projects/gish/internal", now)
	s.Visit("/home/u/projects/gish", now)
	s.Visit("/home/u/gishlike/other", now)

	// The last term must match the final component.
	got := s.Query([]string{"gish"}, now)
	if len(got) != 1 || got[0].Dir != "/home/u/projects/gish" {
		t.Errorf("last-component rule: %+v", got)
	}
	// Earlier terms match in path order.
	if got = s.Query([]string{"proj", "internal"}, now); len(got) != 1 {
		t.Errorf("ordered terms: %+v", got)
	}
	if got = s.Query([]string{"internal", "proj"}, now); len(got) != 0 {
		t.Errorf("out-of-order terms matched: %+v", got)
	}
	// Case-insensitive.
	if got = s.Query([]string{"GISH"}, now); len(got) != 1 {
		t.Errorf("case sensitivity crept in: %+v", got)
	}
}

func TestSeedAndPersistence(t *testing.T) {
	t.Parallel()

	real := t.TempDir() // survives the prune
	path := filepath.Join(t.TempDir(), "dirs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Empty() {
		t.Fatal("fresh store not empty")
	}
	s.Seed(map[string]int{real: 5, "/does/not/exist": 9}, time.Now())
	if err := s.Save(time.Now()); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Query(nil, time.Now())
	// The dead directory was pruned at save; the real one persisted.
	if len(got) != 1 || got[0].Dir != real {
		t.Errorf("persisted index = %+v", got)
	}
}

func TestCorruptIndexStartsFresh(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "dirs.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil || !s.Empty() {
		t.Errorf("corrupt index should reset: %v", err)
	}
}
