// Package envtrust records which env-diff proposals the user has
// explicitly allowed (#12). The direnv lesson as a data model: trust is
// (plugin, directory, content-hash) — scoped to one plugin, one
// directory subtree, and one exact diff. A plugin update or an edited
// .envrc-equivalent changes the hash and the proposal pends again;
// nothing ever applies on reputation.
package envtrust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// DefaultPath returns the trust file location:
// $XDG_DATA_HOME/gish/env-trust.json.
func DefaultPath() (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "gish", "env-trust.json"), nil
}

// Entry is one allow record.
type Entry struct {
	Plugin string `json:"plugin"`
	Dir    string `json:"dir"`
	Hash   string `json:"hash"`
}

// Store is the on-disk allow list. Safe for concurrent use; every
// mutation persists immediately — trust must survive the session.
type Store struct {
	path string

	mu      sync.Mutex
	entries []Entry
}

// Open loads the store, creating state lazily — a missing file is an
// empty store, not an error. A corrupt file is an error: failing open
// must not silently drop recorded trust.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &s.entries); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Hash canonicalizes a diff to its trust hash: sorted set pairs and
// sorted unset names, null-delimited, sha256.
func Hash(set map[string]string, unset []string) string {
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, k := range names {
		b.WriteString("set\x00" + k + "\x00" + set[k] + "\x00")
	}
	u := slices.Clone(unset)
	slices.Sort(u)
	for _, k := range u {
		b.WriteString("unset\x00" + k + "\x00")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Trusted reports whether this exact proposal was allowed before.
func (s *Store) Trusted(plugin, dir, hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.entries, Entry{Plugin: plugin, Dir: dir, Hash: hash})
}

// Allow records a proposal and persists. Allowing a new hash for a
// (plugin, dir) replaces the old record — one live allow per pair.
func (s *Store) Allow(plugin, dir, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = slices.DeleteFunc(s.entries, func(e Entry) bool {
		return e.Plugin == plugin && e.Dir == dir
	})
	s.entries = append(s.entries, Entry{Plugin: plugin, Dir: dir, Hash: hash})
	return s.persistLocked()
}

// Revoke removes every record for dir (any plugin, any hash) and
// persists. Reports whether anything was removed.
func (s *Store) Revoke(dir string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.entries)
	s.entries = slices.DeleteFunc(s.entries, func(e Entry) bool { return e.Dir == dir })
	if len(s.entries) == before {
		return false, nil
	}
	return true, s.persistLocked()
}

// Entries returns a copy, sorted by directory then plugin.
func (s *Store) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.entries)
	slices.SortFunc(out, func(a, b Entry) int {
		if c := strings.Compare(a.Dir, b.Dir); c != 0 {
			return c
		}
		return strings.Compare(a.Plugin, b.Plugin)
	})
	return out
}

func (s *Store) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename: a crash mid-write must not corrupt trust.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
