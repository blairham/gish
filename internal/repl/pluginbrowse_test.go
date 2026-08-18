package repl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/manifest"
)

func boolp(b bool) *bool { return &b }

// Without a terminal, browse must degrade to the plain listing and emit
// zero escape bytes — the same invariant TestHeadlessSurfacesEmitNoEscapes
// holds for every other styled surface. A script that runs `plugin
// browse` gets information, not a hung form.
func TestPluginBrowseDegradesWithoutTerminal(t *testing.T) {
	m := &manifest.Manifest{Plugins: []manifest.Plugin{
		{Source: "zsh-users/zsh-autosuggestions"},
		{Source: "junegunn/fzf", Kind: manifest.KindRelease, Pin: "0.55.0", Enabled: boolp(false)},
	}}

	var out, errb bytes.Buffer
	hc := handlerIO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}

	if got := runPluginBrowse(hc, filepath.Join(t.TempDir(), "plugins.toml"), m); got[0] != "true" {
		t.Fatalf("browse returned %v", got)
	}
	if bytes.ContainsRune(out.Bytes(), 0x1b) {
		t.Errorf("escape bytes leaked into headless output: %q", out.String())
	}
	for _, want := range []string{"zsh-autosuggestions", "fzf", "off", "release", "@0.55.0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("listing missing %q:\n%s", want, out.String())
		}
	}
	// A headless browse must not rewrite the manifest.
	if !m.Plugins[0].On() || m.Plugins[1].On() {
		t.Error("headless browse changed enabled state")
	}
}

func TestPluginBrowseEmptyListDegrades(t *testing.T) {
	var out, errb bytes.Buffer
	hc := handlerIO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}

	runPluginBrowse(hc, filepath.Join(t.TempDir(), "plugins.toml"), &manifest.Manifest{})
	if !strings.Contains(out.String(), "no plugins configured") {
		t.Errorf("empty listing = %q", out.String())
	}
	if bytes.ContainsRune(out.Bytes(), 0x1b) {
		t.Errorf("escape bytes in empty listing: %q", out.String())
	}
}

// applyEnabled is the whole edit the multi-select performs, so it is
// tested directly rather than through a form.
func TestApplyEnabled(t *testing.T) {
	m := &manifest.Manifest{Plugins: []manifest.Plugin{
		{Source: "owner/a"},                        // on by default
		{Source: "owner/b"},                        // on by default
		{Source: "owner/c", Enabled: boolp(false)}, // off
	}}

	// Keep a, turn b off, turn c on.
	changed := applyEnabled(m, []string{"a", "c"})
	if changed != 2 {
		t.Errorf("changed = %d, want 2", changed)
	}
	if !m.Plugins[0].On() {
		t.Error("a should still be on")
	}
	if m.Plugins[1].On() {
		t.Error("b should be off")
	}
	if !m.Plugins[2].On() {
		t.Error("c should be on")
	}

	// Idempotent: applying the same selection changes nothing.
	if again := applyEnabled(m, []string{"a", "c"}); again != 0 {
		t.Errorf("re-applying changed %d entries, want 0", again)
	}
}

// Selecting nothing disables everything rather than being read as "no
// selection, leave alone" — the multi-select's empty state is a real
// answer, and treating it as a no-op would make "turn everything off"
// impossible.
func TestApplyEnabledEmptySelectionDisablesAll(t *testing.T) {
	m := &manifest.Manifest{Plugins: []manifest.Plugin{{Source: "owner/a"}, {Source: "owner/b"}}}
	if changed := applyEnabled(m, nil); changed != 2 {
		t.Fatalf("changed = %d, want 2", changed)
	}
	for _, p := range m.Plugins {
		if p.On() {
			t.Errorf("%s still on", p.Name())
		}
	}
}

// The saved file has to survive a round trip: the form's whole output is
// this file, and a toggle that does not persist is the worst kind of
// silent failure.
func TestApplyEnabledPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins.toml")
	m := &manifest.Manifest{Plugins: []manifest.Plugin{{Source: "owner/a"}, {Source: "owner/b"}}}

	applyEnabled(m, []string{"a"})
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}

	reloaded, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Plugins) != 2 {
		t.Fatalf("reloaded %d entries", len(reloaded.Plugins))
	}
	if !reloaded.Plugins[0].On() || reloaded.Plugins[1].On() {
		t.Errorf("enabled state did not survive the round trip: %+v", reloaded.Plugins)
	}
}

func TestBrowseDetail(t *testing.T) {
	tests := []struct {
		name string
		in   manifest.Plugin
		want string
	}{
		{"plain plugin has no detail", manifest.Plugin{Source: "owner/a"}, ""},
		{"release kind shows", manifest.Plugin{Source: "o/a", Kind: manifest.KindRelease}, "release"},
		{"pin shows", manifest.Plugin{Source: "o/a", Pin: "1.2.3"}, "@1.2.3"},
		{"lazy shows", manifest.Plugin{Source: "o/a", Lazy: "command:git"}, "lazy command:git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := browseDetail(tt.in); got != tt.want {
				t.Errorf("browseDetail = %q, want %q", got, tt.want)
			}
		})
	}
}

// The starter list is a starting point, not a registry, and the code
// says so. This keeps a future contributor from growing it into an
// index koi would then be implicitly vouching for.
func TestStarterListStaysSmall(t *testing.T) {
	if len(starterPlugins) > 6 {
		t.Errorf("starter list has %d entries; it is a starting point, not a registry", len(starterPlugins))
	}
	for _, s := range starterPlugins {
		if !strings.Contains(s.source, "/") {
			t.Errorf("starter source %q is not owner/repo", s.source)
		}
		if s.what == "" {
			t.Errorf("starter %q has no explanation", s.source)
		}
	}
}

func TestListPluginsShowsState(t *testing.T) {
	m := &manifest.Manifest{Plugins: []manifest.Plugin{
		{Source: "owner/on-one"},
		{Source: "owner/off-one", Enabled: boolp(false)},
	}}
	var out bytes.Buffer
	listPlugins(handlerIO{Stdout: &out}, m)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[0], "on ") || !strings.HasPrefix(lines[1], "off") {
		t.Errorf("state column wrong:\n%s", out.String())
	}
}

// browsable must agree with the rest of the styled surfaces: a
// non-terminal reader can never host a form.
func TestBrowsableRejectsNonTerminal(t *testing.T) {
	var out bytes.Buffer
	if browsable(strings.NewReader(""), &out) {
		t.Error("a strings.Reader was accepted as a terminal")
	}
	// A real file that is not a terminal (a redirect) must also fail.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if browsable(f, &out) {
		t.Error("/dev/null was accepted as a terminal")
	}
}
