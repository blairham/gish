// Package dirjump is the native zoxide (#94): a frecency index over
// the directories the session actually visits. The shell is the
// tracking point — no prompt hooks, nothing to wire — and a fresh
// index bootstraps from the history store's recorded cwds, so day one
// already knows where the user works.
package dirjump

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// maxEntries caps the index; the lowest-scored entries fall off.
const maxEntries = 1000

// DefaultPath is the index location: $XDG_DATA_HOME/koi/dirs.json.
func DefaultPath() (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "koi", "dirs.json"), nil
}

type entry struct {
	Visits    int       `json:"visits"`
	LastVisit time.Time `json:"last_visit"`
}

// Store is the persistent index. Safe for concurrent use.
type Store struct {
	path string

	mu      sync.Mutex
	entries map[string]entry
	dirty   bool
}

// Open loads the index; a missing file is an empty index.
func Open(path string) (*Store, error) {
	s := &Store{path: path, entries: map[string]entry{}}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &s.entries); err != nil {
		// A corrupt index is derived state: start fresh rather than
		// refuse (contrast env-trust, where silent reset loses consent).
		s.entries = map[string]entry{}
	}
	return s, nil
}

// Empty reports whether anything has been recorded.
func (s *Store) Empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries) == 0
}

// Visit records one arrival at dir.
func (s *Store) Visit(dir string, at time.Time) {
	if dir == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[dir]
	e.Visits++
	if at.After(e.LastVisit) {
		e.LastVisit = at
	}
	s.entries[dir] = e
	s.dirty = true
}

// Seed bulk-loads visit counts (the history bootstrap): counts are
// visits, at is the recency assigned to all of them.
func (s *Store) Seed(counts map[string]int, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for dir, n := range counts {
		if dir == "" || n <= 0 {
			continue
		}
		e := s.entries[dir]
		e.Visits += n
		if at.After(e.LastVisit) {
			e.LastVisit = at
		}
		s.entries[dir] = e
	}
	s.dirty = len(counts) > 0 || s.dirty
}

// Match is one query result.
type Match struct {
	Dir   string
	Score float64
}

// Query returns matches ranked by frecency. Matching is zoxide's rule:
// every term must appear as a case-insensitive substring in path
// order, and the last term must match within the last path component —
// `z proj api` means "the api directory of that project", not any path
// that ever mentions api.
func (s *Store) Query(terms []string, now time.Time) []Match {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Match
	for dir, e := range s.entries {
		if !matches(dir, terms) {
			continue
		}
		out = append(out, Match{Dir: dir, Score: frecency(e, now)})
	}
	slices.SortFunc(out, func(a, b Match) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		default:
			return strings.Compare(a.Dir, b.Dir)
		}
	})
	return out
}

func matches(dir string, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	lower := strings.ToLower(dir)
	// The last term must hit the final path component.
	last := strings.ToLower(terms[len(terms)-1])
	base := strings.ToLower(filepath.Base(dir))
	if !strings.Contains(base, last) {
		return false
	}
	// Every earlier term matches in order over the whole path.
	rest := lower
	for _, term := range terms[:len(terms)-1] {
		i := strings.Index(rest, strings.ToLower(term))
		if i < 0 {
			return false
		}
		rest = rest[i+len(term):]
	}
	return true
}

// frecency is zoxide's shape: visit count weighted by recency bucket.
func frecency(e entry, now time.Time) float64 {
	age := now.Sub(e.LastVisit)
	weight := 0.25
	switch {
	case age < time.Hour:
		weight = 4
	case age < 24*time.Hour:
		weight = 2
	case age < 7*24*time.Hour:
		weight = 0.5
	}
	return float64(e.Visits) * weight
}

// Save persists the index when dirty, pruning directories that no
// longer exist and dropping the lowest-scored entries past the cap.
func (s *Store) Save(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	for dir := range s.entries {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			delete(s.entries, dir)
		}
	}
	if len(s.entries) > maxEntries {
		type scored struct {
			dir   string
			score float64
		}
		all := make([]scored, 0, len(s.entries))
		for dir, e := range s.entries {
			all = append(all, scored{dir, frecency(e, now)})
		}
		slices.SortFunc(all, func(a, b scored) int {
			switch {
			case a.score > b.score:
				return -1
			case a.score < b.score:
				return 1
			default:
				return strings.Compare(a.dir, b.dir)
			}
		})
		for _, victim := range all[maxEntries:] {
			delete(s.entries, victim.dir)
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(s.entries)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}
