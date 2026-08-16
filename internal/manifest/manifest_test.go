package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndDefaults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.toml")
	content := `
[[plugin]]
source = "zsh-users/zsh-autosuggestions"

[[plugin]]
source = "junegunn/fzf"
kind = "release"
pin = "0.55.0"
lazy = "command:fzf"

[[plugin]]
source = "OMZ::plugins/git"

[[plugin]]
source = "someone/disabled-thing"
enabled = false
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Plugins) != 4 {
		t.Fatalf("plugins = %d", len(m.Plugins))
	}

	// Defaults: eager, enabled, source plugin.
	first := m.Plugins[0]
	if !first.On() || first.EffectiveKind() != KindPlugin || first.Lazy != "" {
		t.Errorf("defaults wrong: %+v", first)
	}
	if first.Name() != "zsh-autosuggestions" {
		t.Errorf("name = %q", first.Name())
	}
	// Explicit kind and pin.
	if m.Plugins[1].EffectiveKind() != KindRelease || m.Plugins[1].Pin != "0.55.0" {
		t.Errorf("release entry: %+v", m.Plugins[1])
	}
	// A :: alias infers snippet without being told.
	if m.Plugins[2].EffectiveKind() != KindSnippet {
		t.Errorf("snippet inference failed: %+v", m.Plugins[2])
	}
	if m.Plugins[2].Name() != "git" {
		t.Errorf("snippet name = %q", m.Plugins[2].Name())
	}
	// Disabled stays in the file but off.
	if m.Plugins[3].On() {
		t.Error("disabled entry reported on")
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for name, content := range map[string]string{
		"no source":    "[[plugin]]\npin = \"1.0\"\n",
		"unknown kind": "[[plugin]]\nsource = \"a/b\"\nkind = \"wat\"\n",
		"malformed":    "[[plugin]\nsource =\n",
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".toml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}

	// A missing file is an empty manifest, not an error.
	m, err := Load(filepath.Join(dir, "absent.toml"))
	if err != nil || len(m.Plugins) != 0 {
		t.Errorf("missing file = %+v, %v", m, err)
	}
}

func TestEditAndRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "plugins.toml")
	m := &Manifest{}
	if replaced := m.Add(Plugin{Source: "a/one"}); replaced {
		t.Error("first add reported a replacement")
	}
	m.Add(Plugin{Source: "b/two", Kind: KindRelease, Pin: "1.2.3"})
	// Re-adding the same source replaces rather than duplicating.
	if replaced := m.Add(Plugin{Source: "a/one", Pin: "9"}); !replaced {
		t.Error("re-add did not replace")
	}
	if len(m.Plugins) != 2 {
		t.Fatalf("plugins = %d", len(m.Plugins))
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Plugins) != 2 || reloaded.Plugins[0].Pin != "9" {
		t.Errorf("round trip lost data: %+v", reloaded.Plugins)
	}
	// Find works by short name as well as source.
	if reloaded.Find("two") < 0 || reloaded.Find("b/two") < 0 {
		t.Error("Find by name or source failed")
	}
	if !reloaded.Remove("two") || reloaded.Find("two") >= 0 {
		t.Error("Remove failed")
	}
	if reloaded.Remove("never-existed") {
		t.Error("Remove reported success for a missing entry")
	}
}
