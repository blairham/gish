// Package history is gish's local command history: a JSONL file of
// metadata-rich entries whose shape mirrors the tier-2 plugin contract's
// HistoryEntry (proto/gish/plugin/v1/history.proto), so a HistoryBackend
// plugin observes exactly what the file records. The local file is
// authoritative and works with zero plugins installed.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// loadMax bounds how many trailing entries are loaded into memory; the
// file itself is never truncated.
const loadMax = 10000

// Entry is one executed command. JSON field names match the proto.
type Entry struct {
	Command       string `json:"command"`
	StartedUnixMs int64  `json:"started_unix_ms"`
	DurationMs    int64  `json:"duration_ms"`
	ExitCode      int    `json:"exit_code"`
	Cwd           string `json:"cwd"`
	SessionID     string `json:"session_id"`
}

// DefaultPath returns the history file location: $XDG_DATA_HOME/gish/
// history.jsonl, defaulting XDG_DATA_HOME to ~/.local/share.
func DefaultPath() (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "gish", "history.jsonl"), nil
}

// Store is an append-mostly history file with an in-memory index.
// Appends are single JSONL lines on an O_APPEND handle, so concurrent
// gish sessions interleave whole entries. Safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	f       *os.File
	entries []Entry // oldest first
}

// Open loads the trailing entries of the file at path (creating it and
// its directory as needed) and opens it for appending. Corrupt lines are
// skipped, never fatal — history must not brick the shell.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	s := &Store{f: f}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e Entry
		if json.Unmarshal(scanner.Bytes(), &e) != nil || e.Command == "" {
			continue
		}
		s.entries = append(s.entries, e)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		f.Close()
		return nil, err
	}
	if len(s.entries) > loadMax {
		s.entries = s.entries[len(s.entries)-loadMax:]
	}
	return s, nil
}

// Skip explains why an Append stored nothing.
type Skip int

const (
	SkipNone      Skip = iota // stored
	SkipEmpty                 // empty command
	SkipPrivate               // leading space (ignorespace)
	SkipDuplicate             // immediate duplicate (ignoredups)
	SkipSecret                // matched a secret-scrubbing rule
)

// Append records an executed command, reporting why it was skipped when
// it wasn't stored. Commands starting with a space are private (classic
// ignorespace), immediate duplicates collapse (ignoredups), and commands
// matching a secret-scrubbing rule never reach disk (#10).
func (s *Store) Append(e Entry) (Skip, error) {
	switch {
	case e.Command == "":
		return SkipEmpty, nil
	case strings.HasPrefix(e.Command, " "):
		return SkipPrivate, nil
	case scrubReason(e.Command) != "":
		return SkipSecret, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.entries); n > 0 && s.entries[n-1].Command == e.Command {
		return SkipDuplicate, nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		return SkipNone, err
	}
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return SkipNone, err
	}
	s.entries = append(s.entries, e)
	return SkipNone, nil
}

// Close releases the file handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

// Match returns the nth most-recent distinct command starting with
// prefix (n=0 is the newest). An empty prefix matches everything.
func (s *Store) Match(prefix string, n int) (string, bool) {
	return s.scan(n, func(cmd string) bool {
		return strings.HasPrefix(cmd, prefix)
	})
}

// Search returns the nth most-recent distinct command containing query.
func (s *Store) Search(query string, n int) (string, bool) {
	return s.scan(n, func(cmd string) bool {
		return strings.Contains(cmd, query)
	})
}

// scan walks entries newest-first, deduplicating so each command is
// offered once, at its most recent position.
func (s *Store) scan(n int, match func(string) bool) (string, bool) {
	if n < 0 {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]struct{})
	for i := len(s.entries) - 1; i >= 0; i-- {
		cmd := s.entries[i].Command
		if _, dup := seen[cmd]; dup || !match(cmd) {
			continue
		}
		seen[cmd] = struct{}{}
		if n == 0 {
			return cmd, true
		}
		n--
	}
	return "", false
}
