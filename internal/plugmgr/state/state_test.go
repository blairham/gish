package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blairham/gish/internal/plugmgr/ice"
	"github.com/blairham/gish/internal/plugmgr/spec"
)

// The object manifest is what `zi list` and `Update` read to decide what
// an installed directory *is*. It is derived state — a corrupt one must
// degrade rather than fail — so the interesting assertions are about the
// stub it falls back to and the round trip that has to survive a restart.

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := &spec.Spec{
		Kind: spec.Plugin,
		Raw:  "zsh-users/zsh-autosuggestions",
		User: "zsh-users",
		Repo: "zsh-autosuggestions",
		URL:  "https://github.com/zsh-users/zsh-autosuggestions",
		ID:   "zsh-users---zsh-autosuggestions",
	}
	ices := ice.FromMap(map[string]string{"from": "gh-r", "ver": "v1.2.3"})

	before := time.Now().UTC().Add(-time.Second)
	if err := SaveObject(dir, s, ices); err != nil {
		t.Fatal(err)
	}
	got, err := LoadObject(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got.Kind != "plugin" || got.ID != s.ID || got.URL != s.URL {
		t.Errorf("round trip lost identity: %+v", got)
	}
	if got.Ices["from"] != "gh-r" || got.Ices["ver"] != "v1.2.3" {
		t.Errorf("round trip lost ices: %+v", got.Ices)
	}
	// The timestamp is what makes `zi list` able to say how old an
	// install is; it must survive as UTC rather than as a local time.
	if got.InstalledAt.Before(before) || got.InstalledAt.Location() != time.UTC {
		t.Errorf("InstalledAt = %v (want a recent UTC time)", got.InstalledAt)
	}
}

// A snippet is the one Kind that is not "plugin", and Update branches on
// exactly that string.
func TestSaveObjectRecordsSnippetKind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := &spec.Spec{Kind: spec.Snippet, Raw: "OMZ::lib/git.zsh", ID: "OMZ---lib---git.zsh"}
	if err := SaveObject(dir, s, ice.New()); err != nil {
		t.Fatal(err)
	}
	got, err := LoadObject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "snippet" {
		t.Errorf("Kind = %q, want snippet", got.Kind)
	}
}

func TestLoadObjectFailsWithoutAManifest(t *testing.T) {
	t.Parallel()

	if _, err := LoadObject(t.TempDir()); err == nil {
		t.Error("a directory with no manifest loaded successfully")
	}
}

// ListObjects is the listing behind `zi list`, and it is deliberately
// forgiving: a directory whose manifest is missing or unreadable still
// appears, as a stub named for the directory. Silently dropping it would
// make an install look uninstalled.
func TestListObjectsStubsUnreadableManifests(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	good := filepath.Join(root, "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveObject(good, &spec.Spec{Kind: spec.Plugin, ID: "good", Raw: "u/good"}, ice.New()); err != nil {
		t.Fatal(err)
	}
	// A directory with no manifest at all, and one with a truncated file.
	if err := os.MkdirAll(filepath.Join(root, "bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, ".zi-go.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A plain file beside the directories must be ignored entirely.
	if err := os.WriteFile(filepath.Join(root, "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	objs, err := ListObjects(root, "plugin")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*Object{}
	for _, o := range objs {
		byID[o.ID] = o
	}
	if len(objs) != 3 {
		t.Fatalf("listed %d objects, want 3 (good, bare, broken): %v", len(objs), byID)
	}
	if byID["good"].URL != "" && byID["good"].Kind != "plugin" {
		t.Errorf("the readable manifest was not used: %+v", byID["good"])
	}
	for _, id := range []string{"bare", "broken"} {
		o, ok := byID[id]
		if !ok {
			t.Fatalf("%q missing from the listing", id)
		}
		if !o.InstalledAt.IsZero() {
			t.Errorf("%q should be a stub with no install time: %+v", id, o)
		}
	}
	if _, ok := byID["stray"]; ok {
		t.Error("a plain file was listed as an object")
	}
}

// A missing root is "nothing installed", not an error — the plugins
// directory does not exist until something is installed (#163).
func TestListObjectsToleratesAMissingRoot(t *testing.T) {
	t.Parallel()

	objs, err := ListObjects(filepath.Join(t.TempDir(), "never-created"), "plugin")
	if err != nil {
		t.Errorf("a missing root errored: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("a missing root listed %d objects", len(objs))
	}
}
