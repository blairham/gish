// Package session persists and resurrects shell sessions (#103).
//
// No shell does this. tmux-resurrect and tmux-continuum (10k+ stars
// between them) exist purely to fake it from outside, and iTerm2's
// restore-terminal-contents is the feature Linux users say they envy.
// The shell is the process that actually knows all of it.
//
// Three rules shape the whole package:
//
//   - **A restored environment is a proposal, never a fact.** Landing in
//     a directory is harmless; silently re-applying a set of environment
//     variables from a file written by a previous process is not. The
//     restore path hands the env diff to the #12 trust flow, which is
//     already the thing that asks before changing a session's
//     environment. Nothing here bypasses it.
//   - **Secrets are filtered on the way in, not on the way out.** A
//     session's environment is exactly where an API token lives. Values
//     that look credential-bearing are never written to disk in the
//     first place, so a leaked session file cannot leak them and there
//     is no scrubbing step to forget to call later.
//   - **This is derived state.** A corrupt or unreadable session file is
//     skipped, never repaired and never fatal. Losing session history
//     costs convenience; refusing to start a shell costs everything.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/blairham/gish/internal/history"
)

// Record is one session's restorable state.
//
// Deliberately small: everything here is cheap to collect at a prompt
// and meaningful an hour later. Process state is *not* here — a
// background job's pid is worthless after a reboot, so what persists is
// the command line, as re-runnable text.
type Record struct {
	// ID is the session id, stable for the life of one interactive
	// shell, and the file's own name.
	ID string `json:"id"`
	// Cwd is where the shell was standing.
	Cwd string `json:"cwd"`
	// Env is the session-scoped diff against the login baseline —
	// filtered (see FilterEnv) before it ever reaches this struct.
	Env map[string]string `json:"env,omitempty"`
	// Pins are the active .tool-versions selections (#77), recorded so a
	// restored session reports the same toolchain it had.
	Pins map[string]string `json:"pins,omitempty"`
	// Jobs are background job command lines as re-runnable text.
	// Processes do not survive; their intent does.
	Jobs []string `json:"jobs,omitempty"`
	// LastCommand is what the session last ran, for the picker's detail
	// column. It comes from the history store, which is already
	// secret-gated (#10), so it is safe by construction.
	LastCommand string `json:"last_command,omitempty"`
	// HistoryPos is the session's position in the history file.
	HistoryPos int `json:"history_pos,omitempty"`
	// UpdatedUnixMs is when this record was last written.
	UpdatedUnixMs int64 `json:"updated_unix_ms"`
}

// Age reports how long ago the session was last seen.
func (r Record) Age(now time.Time) time.Duration {
	if r.UpdatedUnixMs == 0 {
		return 0
	}
	return now.Sub(time.UnixMilli(r.UpdatedUnixMs))
}

// Retention bounds. Session records are small, but an unbounded
// directory of them is a slow leak and a growing pile of stale cwds in
// the picker.
const (
	// MaxSessions is how many records survive a prune, newest first.
	MaxSessions = 50
	// MaxAge drops records nobody will ever restore.
	MaxAge = 30 * 24 * time.Hour
)

// secretish matches variable names that look credential-bearing. Same
// vocabulary the env-plugin path uses, kept in step deliberately: a
// name that is too dangerous to hand a plugin is too dangerous to write
// to a file that outlives the process.
var secretish = regexp.MustCompile(`(?i)(secret|token|passw|credential|api[_-]?key|access[_-]?key|private|session[_-]?key)`)

// denied names never belong in a restored environment: process-loader
// hooks, word splitting, startup-file redirection, and gish's own
// knobs. Restoring any of these would let a session file change how the
// next shell loads code.
func denied(name string) bool {
	switch name {
	case "IFS", "ENV", "BASH_ENV", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT":
		return true
	}
	return strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "GISH_")
}

// FilterEnv drops what must never be persisted, returning the survivors
// and the names it removed.
//
// The removal list is returned rather than discarded because the restore
// UI should be able to say "3 variables were not saved" — a user who
// wonders why their token did not come back deserves the answer, and
// the answer is a feature.
func FilterEnv(env map[string]string) (kept map[string]string, removed []string) {
	kept = make(map[string]string, len(env))
	for name, value := range env {
		if denied(name) || secretish.MatchString(name) {
			removed = append(removed, name)
			continue
		}
		kept[name] = value
	}
	sort.Strings(removed)
	return kept, removed
}

// Store is a directory of session records.
type Store struct{ dir string }

// DefaultDir is $XDG_STATE_HOME/gish/sessions. State, not data: these
// records describe a moment in a process's life, and losing them costs
// nothing permanent — unlike history, which is the user's own record.
func DefaultDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "gish", "sessions"), nil
}

// Open returns a store over dir. The directory is created by the first
// Save, not here (#163) — a shell that is opened and closed without
// running anything has no session worth recording, and should leave no
// trace of having been started.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("session: no directory")
	}
	return &Store{dir: dir}, nil
}

// OpenDefault is Open over DefaultDir.
func OpenDefault() (*Store, error) {
	dir, err := DefaultDir()
	if err != nil {
		return nil, err
	}
	return Open(dir)
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, safeID(id)+".json") }

// safeID keeps an id to one path segment. Session ids are ours, but a
// file name derived from anything is worth constraining.
func safeID(id string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '-'
	}, id)
	if clean == "" {
		return "unknown"
	}
	return clean
}

// Save writes a record, filtering its environment first.
//
// Write-then-rename, the same discipline every other piece of gish state
// uses: a shell killed mid-write must never leave a half-written record
// that the next `sessions` call chokes on.
func (s *Store) Save(r Record) error {
	if r.ID == "" {
		return errors.New("session: record has no id")
	}
	r.Env, _ = FilterEnv(r.Env)
	// The command line gets the same gate the history store applies.
	// #10's guarantee is that a secret-bearing command is never
	// recorded, and a second store with its own idea of what is safe is
	// exactly how that guarantee stops being true — a command like
	// `export GITHUB_TOKEN=…` is refused by history and would otherwise
	// be written here verbatim.
	if r.LastCommand != "" && history.SecretReason(r.LastCommand) != "" {
		r.LastCommand = ""
	}
	if r.UpdatedUnixMs == 0 {
		r.UpdatedUnixMs = time.Now().UnixMilli()
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	final := s.path(r.ID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// List returns records newest first, skipping any it cannot read.
func (s *Store) List() []Record {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name())) //nolint:gosec // our own state dir
		if err != nil {
			continue
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil || r.ID == "" {
			// Derived state: a corrupt record is skipped, never repaired
			// and never fatal.
			continue
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b Record) int {
		switch {
		case a.UpdatedUnixMs > b.UpdatedUnixMs:
			return -1
		case a.UpdatedUnixMs < b.UpdatedUnixMs:
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Get returns one record by id prefix, so a user can type the first few
// characters. An ambiguous prefix is an error rather than a guess —
// restoring the wrong session is a confusing way to lose your place.
func (s *Store) Get(idPrefix string) (Record, error) {
	if idPrefix == "" {
		return Record{}, errors.New("no session id given")
	}
	var matches []Record
	for _, r := range s.List() {
		if r.ID == idPrefix {
			return r, nil // an exact id always wins over a prefix
		}
		if strings.HasPrefix(r.ID, idPrefix) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return Record{}, fmt.Errorf("no session matching %q", idPrefix)
	case 1:
		return matches[0], nil
	}
	ids := make([]string, len(matches))
	for i, m := range matches {
		ids[i] = shortID(m.ID)
	}
	return Record{}, fmt.Errorf("%q matches %d sessions: %s", idPrefix, len(matches), strings.Join(ids, " "))
}

// Remove deletes one record.
func (s *Store) Remove(id string) error {
	err := os.Remove(s.path(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Prune drops records past the age or count caps and reports how many
// it removed. Called on exit, where a little I/O costs nobody anything.
func (s *Store) Prune(now time.Time) int {
	records := s.List()
	removed := 0
	for i, r := range records {
		if i >= MaxSessions || r.Age(now) > MaxAge {
			if s.Remove(r.ID) == nil {
				removed++
			}
		}
	}
	return removed
}

// shortID is the display form: enough to identify, short enough to type.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ShortID is the exported display form.
func ShortID(id string) string { return shortID(id) }
