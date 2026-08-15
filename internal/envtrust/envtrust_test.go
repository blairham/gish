package envtrust

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHashCanonical(t *testing.T) {
	t.Parallel()

	a := Hash(map[string]string{"A": "1", "B": "2"}, []string{"X", "Y"})
	b := Hash(map[string]string{"B": "2", "A": "1"}, []string{"Y", "X"})
	if a != b {
		t.Error("hash must be order-independent")
	}
	if a == Hash(map[string]string{"A": "1", "B": "changed"}, []string{"X", "Y"}) {
		t.Error("value change must change the hash")
	}
	if a == Hash(map[string]string{"A": "1", "B": "2"}, []string{"X"}) {
		t.Error("unset change must change the hash")
	}
	// set{K:""} and unset{K} are different proposals.
	if Hash(map[string]string{"K": ""}, nil) == Hash(nil, []string{"K"}) {
		t.Error("set-to-empty and unset must hash differently")
	}
}

func TestStoreRoundtrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sub", "env-trust.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Trusted("p", "/dir", "h1") {
		t.Error("empty store trusts nothing")
	}
	if err := s.Allow("p", "/dir", "h1"); err != nil {
		t.Fatal(err)
	}
	if !s.Trusted("p", "/dir", "h1") {
		t.Error("allow not recorded")
	}

	// Persistence: a fresh open sees the record.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Trusted("p", "/dir", "h1") {
		t.Error("allow not persisted")
	}

	// A new hash replaces the old allow for the same (plugin, dir).
	if err := s2.Allow("p", "/dir", "h2"); err != nil {
		t.Fatal(err)
	}
	if s2.Trusted("p", "/dir", "h1") {
		t.Error("old hash still trusted after re-allow")
	}
	if !s2.Trusted("p", "/dir", "h2") {
		t.Error("new hash not trusted")
	}

	// Revoke drops the directory.
	if removed, err := s2.Revoke("/dir"); err != nil || !removed {
		t.Fatalf("revoke = %v, %v", removed, err)
	}
	if s2.Trusted("p", "/dir", "h2") {
		t.Error("trusted after revoke")
	}
	if removed, _ := s2.Revoke("/dir"); removed {
		t.Error("second revoke should be a no-op")
	}
}

func TestOpenCorruptFileFails(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "env-trust.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("corrupt trust file must fail open, not silently reset")
	}
}
