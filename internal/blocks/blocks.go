// Package blocks stores a command's captured output beside its history
// entry (#99 stage 3).
//
// A block is a command and its output as one unit. The command half
// already exists — history is metadata-rich JSONL with cwd, exit status,
// duration and timing — so this package is only the output half plus the
// reference that ties them together.
//
// Two rules, both learned rather than assumed:
//
//   - **Output is redacted on the way in, never on the way out.** A
//     command's output is a new place for a credential to land, and the
//     #10 rules previously only covered command lines. Redacting at
//     write time means a leaked blocks directory cannot leak a token,
//     and there is no scrubbing step for a later reader to forget.
//     Redaction rather than rejection: dropping a whole build log
//     because one line echoed a token would destroy exactly what the
//     user wanted to see (see history.RedactOutput).
//   - **This is derived state.** A block whose file is missing, corrupt
//     or unreadable degrades to "output not available" — never an error
//     that costs the user their history entry, which is the
//     authoritative record.
package blocks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blairham/gish/internal/history"
)

// Retention bounds. Output is far bulkier than history lines, so it is
// capped by both count and total bytes — a single `find /` should not be
// able to fill a home directory.
const (
	// MaxBlocks is how many command outputs are kept, newest first.
	MaxBlocks = 500
	// MaxTotalBytes caps the directory's total size.
	MaxTotalBytes = 32 << 20 // 32MB
	// MaxAge drops blocks nobody will look at again.
	MaxAge = 30 * 24 * time.Hour
)

// Retention bounds one store. Defaults are the constants above;
// they are fields rather than constants so the policy is testable
// without writing tens of megabytes to prove a cap works.
type Retention struct {
	MaxBlocks     int
	MaxTotalBytes int64
	MaxAge        time.Duration
}

// DefaultRetention is what the shell uses.
func DefaultRetention() Retention {
	return Retention{MaxBlocks: MaxBlocks, MaxTotalBytes: MaxTotalBytes, MaxAge: MaxAge}
}

// Store is a directory of captured outputs, one file per block.
type Store struct {
	dir       string
	retention Retention
}

// WithRetention returns a copy of the store using r.
func (s *Store) WithRetention(r Retention) *Store {
	return &Store{dir: s.dir, retention: r}
}

// DefaultDir is $XDG_DATA_HOME/gish/blocks. Data rather than state: it
// is the user's own command output, the same class as history, and it
// should survive a reboot for as long as retention allows.
func DefaultDir() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "gish", "blocks"), nil
}

// Open returns a store over dir, creating it on demand with owner-only
// permissions — command output is at least as private as history.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("blocks: no directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, retention: DefaultRetention()}, nil
}

// OpenDefault is Open over DefaultDir.
func OpenDefault() (*Store, error) {
	dir, err := DefaultDir()
	if err != nil {
		return nil, err
	}
	return Open(dir)
}

// Ref identifies one stored output. It is what a history entry carries.
type Ref string

// Meta describes a stored output without reading it.
type Meta struct {
	Ref       Ref
	Bytes     int64
	Redacted  int  // credential-shaped spans removed at write time
	Truncated bool // the ring dropped output before this was written
	ModTime   time.Time
}

func (s *Store) path(ref Ref) string { return filepath.Join(s.dir, string(ref)) }

// Put stores output and returns its reference.
//
// The ref is content-addressed, which deduplicates identical output
// (running the same command twice costs one file) and makes the name
// safe by construction — it is a hash, so it can never escape the
// directory however the output was produced.
//
// truncated records that the capture ring dropped bytes before this
// point, so a reader can say the log is partial rather than implying it
// is whole.
func (s *Store) Put(out []byte, truncated bool) (Ref, int, error) {
	clean, redacted := history.RedactOutput(out)
	sum := sha256.Sum256(clean)
	ref := Ref(hex.EncodeToString(sum[:])[:32] + suffix(truncated, redacted))

	final := s.path(ref)
	if _, err := os.Stat(final); err == nil {
		return ref, redacted, nil // identical output already stored
	}
	// Write-then-rename: a shell killed mid-write must not leave a
	// truncated file that a later read would present as the output.
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, clean, 0o600); err != nil {
		return "", redacted, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", redacted, err
	}
	return ref, redacted, nil
}

// suffix encodes the two facts a reader needs before opening the file,
// so a listing can report them without reading every block.
func suffix(truncated bool, redacted int) string {
	var b strings.Builder
	if truncated {
		b.WriteString("-t")
	}
	if redacted > 0 {
		fmt.Fprintf(&b, "-r%d", redacted)
	}
	return b.String()
}

// Get returns stored output. A missing or unreadable block is not an
// error worth propagating — it is derived state, and the history entry
// it belongs to is still perfectly good.
func (s *Store) Get(ref Ref) ([]byte, bool) {
	if ref == "" || strings.ContainsAny(string(ref), `/\`) {
		return nil, false
	}
	data, err := os.ReadFile(s.path(ref)) //nolint:gosec // our own data dir, ref is a hash
	if err != nil {
		return nil, false
	}
	return data, true
}

// Stat reports what is known about a block without reading it.
func (s *Store) Stat(ref Ref) (Meta, bool) {
	fi, err := os.Stat(s.path(ref))
	if err != nil {
		return Meta{}, false
	}
	return metaOf(ref, fi.Size(), fi.ModTime()), true
}

func metaOf(ref Ref, size int64, mod time.Time) Meta {
	m := Meta{Ref: ref, Bytes: size, ModTime: mod}
	name := string(ref)
	m.Truncated = strings.Contains(name, "-t")
	if i := strings.LastIndex(name, "-r"); i >= 0 {
		_, _ = fmt.Sscanf(name[i:], "-r%d", &m.Redacted) //nolint:errcheck // absent count means zero
	}
	return m
}

// List returns stored blocks, newest first.
func (s *Store) List() []Meta {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []Meta
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, metaOf(Ref(e.Name()), fi.Size(), fi.ModTime()))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out
}

// Prune enforces retention by age, then count, then total bytes, and
// reports how many blocks it removed.
//
// Bytes come last on purpose: dropping the oldest is the least
// surprising rule, and a size cap that evicted recent output to make
// room for old output would be worse than useless.
func (s *Store) Prune(now time.Time) int {
	blocks := s.List()
	removed := 0
	var total int64
	for i, b := range blocks {
		switch {
		case now.Sub(b.ModTime) > s.retention.MaxAge, i >= s.retention.MaxBlocks:
			if s.Remove(b.Ref) == nil {
				removed++
			}
		default:
			total += b.Bytes
			if total > s.retention.MaxTotalBytes {
				if s.Remove(b.Ref) == nil {
					removed++
				}
			}
		}
	}
	return removed
}

// Remove deletes one block.
func (s *Store) Remove(ref Ref) error {
	err := os.Remove(s.path(ref))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
