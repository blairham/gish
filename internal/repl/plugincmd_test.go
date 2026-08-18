package repl

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/manifest"
)

// pluginEnv points the manifest at a temp config home and installs a
// manager backed by a fake plugmgr, so the tests never touch a network
// or a real ~/.zi-go.
func pluginEnv(t *testing.T) (*fakePlugmgr, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)

	fake := &fakePlugmgr{payload: filepath.Join(base, "payload.sh")}
	if err := os.WriteFile(fake.payload, []byte("PLUGIN_LOADED=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := newPluginManager(fake)
	if err != nil {
		t.Fatal(err)
	}
	pluginMgr = mgr
	t.Cleanup(func() { pluginMgr = nil })
	return fake, mgr.path
}

func TestPluginAddWritesManifest(t *testing.T) {
	_, path := pluginEnv(t)
	rc := filepath.Join(t.TempDir(), "koirc")

	out, _, err := runConfigScript(t, rc,
		"plugin add zsh-users/zsh-autosuggestions\n"+
			"plugin add junegunn/fzf --kind release --pin 0.55.0\n"+
			"plugin\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "added zsh-users/zsh-autosuggestions") {
		t.Errorf("no add confirmation: %q", out)
	}
	if !strings.Contains(out, "junegunn/fzf") || !strings.Contains(out, "release") {
		t.Errorf("listing missing the release entry: %q", out)
	}

	m, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Plugins) != 2 {
		t.Fatalf("manifest = %+v", m.Plugins)
	}
	if m.Plugins[1].Kind != manifest.KindRelease || m.Plugins[1].Pin != "0.55.0" {
		t.Errorf("release entry not persisted: %+v", m.Plugins[1])
	}
}

func TestPluginPinDisableRemove(t *testing.T) {
	_, path := pluginEnv(t)
	rc := filepath.Join(t.TempDir(), "koirc")

	out, _, err := runConfigScript(t, rc,
		"plugin add a/one\nplugin pin one 2.0\nplugin disable one\nplugin\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("listing does not show disabled: %q", out)
	}
	m, _ := manifest.Load(path)
	if m.Plugins[0].Pin != "2.0" || m.Plugins[0].On() {
		t.Errorf("pin/disable not persisted: %+v", m.Plugins[0])
	}

	if _, _, err = runConfigScript(t, rc, "plugin remove one\n"); err != nil {
		t.Fatal(err)
	}
	m, _ = manifest.Load(path)
	if len(m.Plugins) != 0 {
		t.Errorf("remove did not persist: %+v", m.Plugins)
	}
}

func TestPluginRejectsUnknownFlagsAndNames(t *testing.T) {
	pluginEnv(t)
	rc := filepath.Join(t.TempDir(), "koirc")

	for script, want := range map[string]string{
		"plugin add a/b --kind wat\n":     "unknown kind",
		"plugin add a/b --lazy startup\n": "command:NAME",
		"plugin add a/b --bogus x\n":      "unknown flag",
		"plugin pin nothere 1\n":          "not in the manifest",
		"plugin frobnicate\n":             "unknown arguments",
	} {
		_, errOut, _ := runConfigScript(t, rc, script)
		if !strings.Contains(errOut, want) {
			t.Errorf("%q: stderr = %q, want %q", script, errOut, want)
		}
	}
}

func TestLazyEntryDefersUntilTrigger(t *testing.T) {
	fake, _ := pluginEnv(t)

	// A lazy entry registers its trigger instead of loading.
	pluginMgr.man.Add(manifest.Plugin{Source: "a/lazy-one", Lazy: "command:mytrigger"})
	if lines := pluginMgr.loadEager(); len(lines) != 0 {
		t.Fatalf("lazy entry loaded eagerly: %v", lines)
	}
	if fake.loads != 0 {
		t.Fatalf("engine called for a lazy entry: %d", fake.loads)
	}

	// The trigger claims it exactly once.
	entry, ok := pluginMgr.take("mytrigger")
	if !ok || entry.Source != "a/lazy-one" {
		t.Fatalf("trigger did not fire: %+v %v", entry, ok)
	}
	if _, again := pluginMgr.take("mytrigger"); again {
		t.Error("trigger fired twice: a plugin would load on every use")
	}
}

func TestEagerEntriesLoadAndDisabledDoNot(t *testing.T) {
	fake, _ := pluginEnv(t)
	off := false
	pluginMgr.man.Add(manifest.Plugin{Source: "a/eager"})
	pluginMgr.man.Add(manifest.Plugin{Source: "a/off", Enabled: &off})

	lines := pluginMgr.loadEager()
	if len(lines) != 1 {
		t.Fatalf("expected one payload, got %v", lines)
	}
	if fake.loads != 1 {
		t.Errorf("engine loads = %d, want 1 (disabled entry must be skipped)", fake.loads)
	}
}

// fakePlugmgr records calls without touching disk or network.
type fakePlugmgr struct {
	payload string
	loads   int
	ices    []string
}

func (f *fakePlugmgr) SetIces(args []string) error { f.ices = args; return nil }
func (f *fakePlugmgr) Load(string) (string, error) { f.loads++; return f.payload, nil }
func (f *fakePlugmgr) Snippet(string) (string, error) {
	f.loads++
	return f.payload, nil
}
func (f *fakePlugmgr) Update(string, io.Writer) error { return nil }
func (f *fakePlugmgr) Delete(string) error            { return nil }
func (f *fakePlugmgr) List(io.Writer) error           { return nil }
