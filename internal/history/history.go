// Package history is koi's local command history: a JSONL file of
// metadata-rich entries whose shape mirrors the tier-2 plugin contract's
// HistoryEntry (proto/koi/plugin/v1/history.proto), so a HistoryBackend
// plugin observes exactly what the file records. The local file is
// authoritative and works with zero plugins installed.
package history

import (
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
	// Block references this command's captured output (#99 stage 3),
	// empty when capture was off or produced nothing. Purely a
	// reference: the output lives in the blocks store, so a history file
	// stays small and readable, and a missing block costs the output
	// rather than the entry.
	Block string `json:"block,omitempty"`
}

// DefaultPath returns the history file location: $XDG_DATA_HOME/koi/
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
	return filepath.Join(dataHome, "koi", "history.jsonl"), nil
}

// Store is an append-mostly history file with an in-memory index.
// Appends are single JSONL lines on an O_APPEND handle, so concurrent
// koi sessions interleave whole entries. Safe for concurrent use.
//
// Live cross-session history (#40): lookups reload the file tail first,
// so commands from concurrent sessions appear here as they happen. One
// stat when nothing changed; own-session entries are skipped on reload
// (they are already in memory).
type Store struct {
	mu      sync.RWMutex
	f       *os.File
	path    string
	session string
	loaded  int64   // bytes of the file already ingested (complete lines)
	entries []Entry // oldest first
}

// SetSession identifies this session's entries so reloads skip them.
func (s *Store) SetSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = id
}

// Open loads the trailing entries of the file at path. Nothing is
// created until the first Append; a missing file is an empty history,
// not an error. Corrupt lines are skipped, never fatal — history must
// not brick the shell.
func Open(path string) (*Store, error) {
	s := &Store{path: path}

	// Nothing is created here (#163). A session that is started and
	// closed without running a command has asked for nothing and must
	// leave nothing: no data directory, no empty history file. The file
	// appears on the first command worth remembering, which is also the
	// first moment the user would expect a shell to have written
	// anything.
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = nil // first run: an empty history is not an error
	} else if err != nil {
		return nil, err
	}
	entries, consumed := consumeLines(data, "")
	s.entries = entries
	s.loaded = consumed
	if len(s.entries) > loadMax {
		s.entries = s.entries[len(s.entries)-loadMax:]
	}
	return s, nil
}

// consumeLines parses complete JSONL lines, skipping corrupt ones and
// entries from skipSession. It reports how many bytes were consumed —
// a trailing partial line (another session mid-write) stays unconsumed
// for the next reload.
func consumeLines(data []byte, skipSession string) ([]Entry, int64) {
	var entries []Entry
	var consumed int64
	for {
		nl := bytesIndexByte(data[consumed:], '\n')
		if nl < 0 {
			break
		}
		line := data[consumed : consumed+int64(nl)]
		consumed += int64(nl) + 1
		var e Entry
		if json.Unmarshal(line, &e) != nil || e.Command == "" {
			continue
		}
		if skipSession != "" && e.SessionID == skipSession {
			continue
		}
		entries = append(entries, e)
	}
	return entries, consumed
}

func bytesIndexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// reload ingests lines other sessions appended since the last look.
// Cheap when nothing changed: one stat. Callers hold no lock.
func (s *Store) reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fi, err := os.Stat(s.path)
	if err != nil || fi.Size() <= s.loaded {
		return
	}
	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(s.loaded, 0); err != nil {
		return
	}
	data := make([]byte, fi.Size()-s.loaded)
	n, _ := f.Read(data)
	fresh, consumed := consumeLines(data[:n], s.session)
	s.loaded += consumed
	s.entries = append(s.entries, fresh...)
	if len(s.entries) > loadMax {
		s.entries = s.entries[len(s.entries)-loadMax:]
	}
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
	if err := s.openForAppend(); err != nil {
		return SkipNone, err
	}
	pre, _ := s.f.Seek(0, 2) //nolint:errcheck // best-effort accounting
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return SkipNone, err
	}
	if pre == s.loaded {
		// No interleaved writes from other sessions: our line extends
		// the ingested region directly.
		s.loaded += int64(len(line)) + 1
	}
	s.entries = append(s.entries, e)
	return SkipNone, nil
}

// openForAppend creates the directory and file on first write. Callers
// hold the lock.
func (s *Store) openForAppend() error {
	if s.f != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	s.f = f
	return nil
}

// Close releases the file handle. A store that never wrote never opened
// one, and closing it is not an error.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}

// Match returns the nth most-recent distinct command starting with
// prefix (n=0 is the newest). An empty prefix matches everything.
func (s *Store) Match(prefix string, n int) (string, bool) {
	s.reload()
	return s.scan(n, func(cmd string) bool {
		return strings.HasPrefix(cmd, prefix)
	})
}

// Search returns the nth most-recent distinct command containing query.
func (s *Store) Search(query string, n int) (string, bool) {
	s.reload()
	return s.scan(n, func(cmd string) bool {
		return strings.Contains(cmd, query)
	})
}

// Recent returns up to n distinct commands, newest first. Because the
// store never records secret-bearing commands (#10), the result is
// safe to hand to an AIProvider as context (#20).
func (s *Store) Recent(n int) []string {
	s.reload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, n)
	seen := make(map[string]struct{})
	for i := len(s.entries) - 1; i >= 0 && len(out) < n; i-- {
		cmd := s.entries[i].Command
		if _, dup := seen[cmd]; dup {
			continue
		}
		seen[cmd] = struct{}{}
		out = append(out, cmd)
	}
	return out
}

// DirCounts tallies how many recorded commands ran in each directory —
// the native-z bootstrap (#94): a fresh jump index seeds from where
// the user has actually been working.
func (s *Store) DirCounts() map[string]int {
	s.reload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{}
	for _, e := range s.entries {
		if e.Cwd != "" {
			out[e.Cwd]++
		}
	}
	return out
}

// RecentEntries returns up to n distinct entries, newest first, with
// their metadata — what the ctrl-r picker shows so a choice is
// informed rather than a guess (#100).
func (s *Store) RecentEntries(n int) []Entry {
	s.reload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, n)
	seen := make(map[string]struct{})
	for i := len(s.entries) - 1; i >= 0 && len(out) < n; i-- {
		e := s.entries[i]
		if _, dup := seen[e.Command]; dup {
			continue
		}
		seen[e.Command] = struct{}{}
		out = append(out, e)
	}
	return out
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
