package blocks

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The guarantee that matters: a credential in command output never
// reaches disk. Asserted against the bytes in the file, because that is
// what a reader of the directory can see.
func TestSecretsAreRedactedBeforeTheyReachDisk(t *testing.T) {
	s := testStore(t)
	log := []byte("building\nGITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz012345\nfailed: missing symbol\n")

	ref, redacted, err := s.Put(log, false)
	if err != nil {
		t.Fatal(err)
	}
	if redacted == 0 {
		t.Error("Put reported no redaction")
	}

	onDisk, err := os.ReadFile(filepath.Join(s.dir, string(ref)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, []byte("ghp_abcdefghijklmnopqrstuvwxyz012345")) {
		t.Errorf("token reached disk:\n%s", onDisk)
	}
	// The rest of the log is the reason the feature exists.
	for _, keep := range []string{"building", "failed: missing symbol"} {
		if !bytes.Contains(onDisk, []byte(keep)) {
			t.Errorf("redaction ate %q:\n%s", keep, onDisk)
		}
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	s := testStore(t)
	out := []byte("hello\nworld\n")

	ref, _, err := s.Put(out, false)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(ref)
	if !ok {
		t.Fatal("stored block could not be read back")
	}
	if !bytes.Equal(got, out) {
		t.Errorf("got %q, want %q", got, out)
	}
}

// Identical output costs one file: running the same command twice is
// the common case, not the exception.
func TestIdenticalOutputIsStoredOnce(t *testing.T) {
	s := testStore(t)
	out := []byte("same output")

	a, _, err := s.Put(out, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.Put(out, false)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("same output produced two refs: %s and %s", a, b)
	}
	if got := len(s.List()); got != 1 {
		t.Errorf("stored %d files for identical output", got)
	}
}

// A reader has to be able to tell a partial log from a whole one, and a
// redacted one from a pristine one, without opening the file.
func TestMetaCarriesTruncationAndRedaction(t *testing.T) {
	s := testStore(t)

	ref, redacted, err := s.Put([]byte("api_key=supersecretvalue123\ntail\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if redacted == 0 {
		t.Fatal("nothing redacted")
	}
	meta, ok := s.Stat(ref)
	if !ok {
		t.Fatal("Stat failed")
	}
	if !meta.Truncated {
		t.Error("truncation not recorded; a partial log would read as whole")
	}
	if meta.Redacted != redacted {
		t.Errorf("meta reports %d redactions, Put reported %d", meta.Redacted, redacted)
	}
}

// Derived state: a missing block costs the output, never the history
// entry it belongs to.
func TestMissingBlockDegradesQuietly(t *testing.T) {
	s := testStore(t)
	if _, ok := s.Get("nonexistent"); ok {
		t.Error("reading a missing block reported success")
	}
	if _, ok := s.Stat("nonexistent"); ok {
		t.Error("Stat on a missing block reported success")
	}
}

// A ref is a file name, so it can never address anything outside the
// store however it was produced.
func TestRefCannotEscapeTheDirectory(t *testing.T) {
	s := testStore(t)
	for _, bad := range []Ref{"../../etc/passwd", "sub/dir", `..\..\win`} {
		if _, ok := s.Get(bad); ok {
			t.Errorf("ref %q escaped the store", bad)
		}
	}
}

// A partial write must be invisible: .tmp is not a block.
func TestPartialWriteIsNotListed(t *testing.T) {
	s := testStore(t)
	if err := os.WriteFile(filepath.Join(s.dir, "abc.tmp"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := len(s.List()); got != 0 {
		t.Errorf("a partial write was listed: %+v", s.List())
	}
}

func TestPruneByAgeAndCount(t *testing.T) {
	// Small caps: the policy is what is under test, not the ability to
	// write five hundred files.
	s := testStore(t).WithRetention(Retention{MaxBlocks: 5, MaxTotalBytes: 1 << 20, MaxAge: MaxAge})
	now := time.Now()

	stale, _, err := s.Put([]byte("old output"), false)
	if err != nil {
		t.Fatal(err)
	}
	old := now.Add(-MaxAge - time.Hour)
	if err := os.Chtimes(s.path(stale), old, old); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		if _, _, err := s.Put([]byte("output "+strings.Repeat("x", i)), false); err != nil {
			t.Fatal(err)
		}
	}

	if removed := s.Prune(now); removed == 0 {
		t.Fatal("prune removed nothing")
	}
	if got := len(s.List()); got > 5 {
		t.Errorf("%d blocks survived the cap of 5", got)
	}
	if _, ok := s.Get(stale); ok {
		t.Error("a block past MaxAge survived")
	}
}

// The byte cap evicts the oldest, never the newest — a size cap that
// dropped recent output to keep old output would be worse than useless.
func TestPruneKeepsTheNewestUnderTheByteCap(t *testing.T) {
	s := testStore(t).WithRetention(Retention{MaxBlocks: 1000, MaxTotalBytes: 8 << 10, MaxAge: MaxAge})
	big := bytes.Repeat([]byte("x"), 1<<10)

	var refs []Ref
	for i := range 20 {
		ref, _, err := s.Put(append(big, byte(i)), false)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
		// Distinct mtimes so "newest" is well defined.
		when := time.Now().Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(s.path(ref), when, when); err != nil {
			t.Fatal(err)
		}
	}
	s.Prune(time.Now().Add(time.Hour))

	if _, ok := s.Get(refs[len(refs)-1]); !ok {
		t.Error("the newest block was evicted by the byte cap")
	}
}
