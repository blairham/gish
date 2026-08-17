package session

import (
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

// The guarantee that matters most: a session's environment is exactly
// where a token lives, and this file outlives the process that wrote
// it. Filtering happens on the way in, so a leaked session file cannot
// leak a credential and there is no scrubbing step to forget later.
func TestSecretsAreNeverWritten(t *testing.T) {
	s := testStore(t)
	err := s.Save(Record{
		ID:  "sess-1",
		Cwd: "/home/me/project",
		Env: map[string]string{
			"PROJECT":               "demo",
			"AWS_SECRET_ACCESS_KEY": "AKIAsecret",
			"GITHUB_TOKEN":          "ghp_secret",
			"MY_API_KEY":            "k",
			"DB_PASSWORD":           "hunter2",
			"SESSION_KEY":           "s",
			"PRIVATE_THING":         "p",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Assert on the bytes on disk, not on the returned struct: what
	// matters is what a reader of the file can see.
	data, err := os.ReadFile(filepath.Join(s.dir, "sess-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"AKIAsecret", "ghp_secret", "hunter2", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("session file contains %q:\n%s", leak, data)
		}
	}
	if !strings.Contains(string(data), "PROJECT") {
		t.Errorf("ordinary variable was dropped:\n%s", data)
	}
}

// Loader hooks are a different hazard from secrets: restoring one would
// let a file written by a previous process change how the next shell
// loads code.
func TestLoaderHooksAreNeverWritten(t *testing.T) {
	kept, removed := FilterEnv(map[string]string{
		"LD_PRELOAD":            "/evil.so",
		"DYLD_INSERT_LIBRARIES": "/evil.dylib",
		"IFS":                   ":",
		"BASH_ENV":              "/tmp/x",
		"GISH_THEME":            "p10k",
		"EDITOR":                "vim",
	})
	if len(kept) != 1 || kept["EDITOR"] != "vim" {
		t.Errorf("kept = %+v, want just EDITOR", kept)
	}
	for _, want := range []string{"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "IFS", "BASH_ENV", "GISH_THEME"} {
		found := false
		for _, r := range removed {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was not reported as removed", want)
		}
	}
}

// The removal list is returned so the UI can say "3 variables were not
// saved" — a user wondering why their token did not come back deserves
// the answer.
func TestFilterReportsWhatItRemoved(t *testing.T) {
	_, removed := FilterEnv(map[string]string{"GITHUB_TOKEN": "x", "OK": "y"})
	if len(removed) != 1 || removed[0] != "GITHUB_TOKEN" {
		t.Errorf("removed = %v, want [GITHUB_TOKEN]", removed)
	}
}

func TestSaveListRoundTrip(t *testing.T) {
	s := testStore(t)
	for _, r := range []Record{
		{ID: "old", Cwd: "/a", UpdatedUnixMs: 1000, LastCommand: "make"},
		{ID: "new", Cwd: "/b", UpdatedUnixMs: 3000, LastCommand: "go test"},
		{ID: "mid", Cwd: "/c", UpdatedUnixMs: 2000},
	} {
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	got := s.List()
	if len(got) != 3 {
		t.Fatalf("listed %d records", len(got))
	}
	if got[0].ID != "new" || got[1].ID != "mid" || got[2].ID != "old" {
		t.Errorf("not newest-first: %s %s %s", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Cwd != "/b" || got[0].LastCommand != "go test" {
		t.Errorf("fields lost in round trip: %+v", got[0])
	}
}

// Derived state: a corrupt record is skipped, never fatal, and never
// takes its neighbors with it.
func TestCorruptRecordIsSkippedNotFatal(t *testing.T) {
	s := testStore(t)
	if err := s.Save(Record{ID: "good", Cwd: "/a", UpdatedUnixMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file with valid JSON but no id is equally unusable.
	if err := os.WriteFile(filepath.Join(s.dir, "empty.json"), []byte(`{"cwd":"/x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := s.List()
	if len(got) != 1 || got[0].ID != "good" {
		t.Errorf("corrupt records were not skipped cleanly: %+v", got)
	}
}

// A half-written record must never be visible. Write-then-rename means
// the .tmp is not a .json, so List cannot see it.
func TestPartialWriteIsInvisible(t *testing.T) {
	s := testStore(t)
	if err := os.WriteFile(filepath.Join(s.dir, "x.json.tmp"), []byte(`{"id":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 0 {
		t.Errorf("a partial write was listed: %+v", got)
	}
}

func TestGetByPrefix(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"abc123", "abc999", "zzz111"} {
		if err := s.Save(Record{ID: id, UpdatedUnixMs: 1}); err != nil {
			t.Fatal(err)
		}
	}

	if r, err := s.Get("zzz"); err != nil || r.ID != "zzz111" {
		t.Errorf("unique prefix: %+v %v", r, err)
	}
	// An exact id wins even when it prefixes another.
	if err := s.Save(Record{ID: "abc", UpdatedUnixMs: 1}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Get("abc"); err != nil || r.ID != "abc" {
		t.Errorf("exact id did not win: %+v %v", r, err)
	}
	// An ambiguous prefix is an error, not a guess: restoring the wrong
	// session is a confusing way to lose your place.
	if _, err := s.Get("abc1"); err != nil {
		t.Errorf("unique longer prefix failed: %v", err)
	}
	if _, err := s.Get("ab"); err == nil {
		t.Error("ambiguous prefix silently picked one")
	}
	if _, err := s.Get("nope"); err == nil {
		t.Error("missing session returned success")
	}
}

func TestPruneByCountAndAge(t *testing.T) {
	s := testStore(t)
	now := time.Now()

	// One stale record, plus more than the cap of fresh ones.
	if err := s.Save(Record{ID: "stale", UpdatedUnixMs: now.Add(-MaxAge - time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	for i := range MaxSessions + 5 {
		if err := s.Save(Record{
			ID:            "fresh-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			UpdatedUnixMs: now.Add(-time.Duration(i) * time.Minute).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if removed := s.Prune(now); removed == 0 {
		t.Fatal("prune removed nothing")
	}
	after := s.List()
	if len(after) > MaxSessions {
		t.Errorf("%d records survived the cap of %d", len(after), MaxSessions)
	}
	for _, r := range after {
		if r.ID == "stale" {
			t.Error("a record past MaxAge survived")
		}
	}
}

func TestSaveRejectsRecordWithoutID(t *testing.T) {
	if err := testStore(t).Save(Record{Cwd: "/a"}); err == nil {
		t.Error("saved a record with no id")
	}
}

// An id is a file name, so it stays one path segment whatever it holds.
func TestIDCannotEscapeTheDirectory(t *testing.T) {
	s := testStore(t)
	if err := s.Save(Record{ID: "../../escape", UpdatedUnixMs: 1}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one file in the store, got %d", len(entries))
	}
	if strings.Contains(entries[0].Name(), "/") || strings.Contains(entries[0].Name(), "..") {
		t.Errorf("id escaped into %q", entries[0].Name())
	}
}

func TestAgeAndShortID(t *testing.T) {
	now := time.Now()
	r := Record{UpdatedUnixMs: now.Add(-90 * time.Minute).UnixMilli()}
	if got := r.Age(now); got < 89*time.Minute || got > 91*time.Minute {
		t.Errorf("age = %s", got)
	}
	if (Record{}).Age(now) != 0 {
		t.Error("a record with no timestamp should report no age")
	}
	if got := ShortID("0123456789abcdef"); got != "01234567" {
		t.Errorf("ShortID = %q", got)
	}
	if got := ShortID("abc"); got != "abc" {
		t.Errorf("short ids pass through unchanged, got %q", got)
	}
}

// A secret-bearing command must not be written here either. #10's
// guarantee is that such a command is never recorded, and history
// refusing it while session restore writes it verbatim would make the
// guarantee false in the only way that matters — the credential is on
// disk regardless of which file it landed in.
//
// Found by running a real secret through a real shell: the token was
// absent from history and from the blocks store, and present in the
// session record.
func TestSecretBearingCommandIsNotRecorded(t *testing.T) {
	s := testStore(t)
	if err := s.Save(Record{
		ID:          "sess-1",
		Cwd:         "/tmp",
		LastCommand: "export GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz012345",
	}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(s.dir, "sess-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ghp_abcdefghijklmnopqrstuvwxyz012345") {
		t.Errorf("session record carries a credential:\n%s", data)
	}

	// The rest of the record still survives: dropping the command is
	// proportionate, dropping the session is not.
	got := s.List()
	if len(got) != 1 || got[0].Cwd != "/tmp" {
		t.Errorf("the whole record was discarded: %+v", got)
	}
	if got[0].LastCommand != "" {
		t.Errorf("LastCommand = %q, want it dropped", got[0].LastCommand)
	}
}

// An ordinary command is still recorded — the gate must not swallow
// everything that merely mentions a variable name.
func TestOrdinaryCommandIsStillRecorded(t *testing.T) {
	s := testStore(t)
	if err := s.Save(Record{ID: "sess-2", LastCommand: "go test ./..."}); err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 1 || got[0].LastCommand != "go test ./..." {
		t.Errorf("ordinary command was dropped: %+v", got)
	}
}
