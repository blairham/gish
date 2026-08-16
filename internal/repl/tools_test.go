package repl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

// toolsHarness: a fake asdf tree, two directories (one pinned), and a
// runner whose PATH starts known.
type toolsHarness struct {
	mgr      *toolsManager
	runner   *interp.Runner
	notices  *strings.Builder
	pinned   string // has .tool-versions
	plain    string // has none
	goBin    string // the install's bin dir
	basePath string // the fixture PATH before any prepends
}

func newToolsHarness(t *testing.T) *toolsHarness {
	t.Helper()
	base := t.TempDir()
	t.Setenv("ASDF_DATA_DIR", filepath.Join(base, "asdf"))
	t.Setenv("HOME", base) // no global .tool-versions fallback
	t.Setenv("USERPROFILE", base) // UserHomeDir reads this on Windows

	h := &toolsHarness{
		notices: &strings.Builder{},
		runner:  newTestRunner(t),
		pinned:  filepath.Join(base, "proj"),
		plain:   filepath.Join(base, "plain"),
		goBin:   filepath.Join(base, "asdf", "installs", "golang", "1.26.6", "bin"),
	}
	h.mgr = newToolsManager(h.notices)
	for _, dir := range []string{h.pinned, h.plain, h.goBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pins := "golang 1.26.6\nnodejs 22.0.0\n"
	if err := os.WriteFile(filepath.Join(h.pinned, ".tool-versions"), []byte(pins), 0o600); err != nil {
		t.Fatal(err)
	}
	// Two entries joined with the platform separator — a ":"-joined
	// literal would read as one entry on Windows.
	h.basePath = "/usr/bin" + string(os.PathListSeparator) + "/bin"
	if err := runEnvScript(t.Context(), h.runner, "export PATH="+quoteArg(t, h.basePath)+"\n"); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *toolsHarness) cd(t *testing.T, dir string) {
	t.Helper()
	if err := runEnvScript(t.Context(), h.runner, "cd "+quoteArg(t, dir)+"\n"); err != nil {
		t.Fatal(err)
	}
	h.mgr.atPrompt(t.Context(), h.runner)
}

func (h *toolsHarness) path() string {
	return h.runner.Vars["PATH"].String()
}

func TestToolsSwitchOnDirChange(t *testing.T) {
	h := newToolsHarness(t)

	h.cd(t, h.pinned)
	if !strings.HasPrefix(h.path(), h.goBin+string(os.PathListSeparator)) {
		t.Fatalf("pinned bin not prepended: %q", h.path())
	}
	if !strings.Contains(h.notices.String(), "nodejs 22.0.0 but it is not installed") {
		t.Errorf("missing-pin notice absent: %q", h.notices.String())
	}

	// Leaving the pinned tree restores the original PATH.
	h.cd(t, h.plain)
	if h.path() != h.basePath {
		t.Fatalf("PATH not restored: %q (applied=%q lastDir=%q)", h.path(), h.mgr.applied, h.mgr.lastDir)
	}

	// Re-entering warns only once per file+pin.
	before := h.notices.Len()
	h.cd(t, h.pinned)
	if h.notices.Len() != before {
		t.Errorf("missing-pin notice repeated: %q", h.notices.String())
	}
}

func TestToolsPreserveUserPathEdits(t *testing.T) {
	h := newToolsHarness(t)
	h.cd(t, h.pinned)

	// The user prepends their own entry mid-session, with the platform
	// list separator.
	sep := string(os.PathListSeparator)
	if err := runEnvScript(t.Context(), h.runner, `export PATH="/opt/mine`+sep+`$PATH"`+"\n"); err != nil {
		t.Fatal(err)
	}
	h.cd(t, h.plain)
	if got := h.path(); got != "/opt/mine"+sep+h.basePath {
		t.Fatalf("user PATH edit lost on revert: %q (applied=%q)", got, h.mgr.applied)
	}
}

func TestToolsDisabledByConfig(t *testing.T) {
	h := newToolsHarness(t)
	if err := runEnvScript(t.Context(), h.runner, "GISH_TOOLS=off\n"); err != nil {
		t.Fatal(err)
	}
	h.cd(t, h.pinned)
	if h.path() != h.basePath {
		t.Fatalf("GISH_TOOLS=off still touched PATH: %q", h.path())
	}

	// Toggling back on re-resolves the same directory.
	if err := runEnvScript(t.Context(), h.runner, "GISH_TOOLS=on\n"); err != nil {
		t.Fatal(err)
	}
	h.mgr.atPrompt(t.Context(), h.runner)
	if !strings.HasPrefix(h.path(), h.goBin) {
		t.Fatalf("re-enable did not re-resolve: %q", h.path())
	}
}

// exeFixture names a fake executable for this platform: Windows keys
// executables on extension, not mode.
func exeFixture(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
