package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

// toolsHarness: a fake asdf tree, two directories (one pinned), and a
// runner whose PATH starts known.
type toolsHarness struct {
	mgr     *toolsManager
	runner  *interp.Runner
	notices *strings.Builder
	pinned  string // has .tool-versions
	plain   string // has none
	goBin   string // the install's bin dir
}

func newToolsHarness(t *testing.T) *toolsHarness {
	t.Helper()
	base := t.TempDir()
	t.Setenv("ASDF_DATA_DIR", filepath.Join(base, "asdf"))
	t.Setenv("HOME", base) // no global .tool-versions fallback

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
	if err := runEnvScript(t.Context(), h.runner, "export PATH=/usr/bin:/bin\n"); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *toolsHarness) cd(t *testing.T, dir string) {
	t.Helper()
	if err := runEnvScript(t.Context(), h.runner, "cd "+dir+"\n"); err != nil {
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
	if h.path() != "/usr/bin:/bin" {
		t.Fatalf("PATH not restored: %q", h.path())
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

	// The user prepends their own entry mid-session.
	if err := runEnvScript(t.Context(), h.runner, `export PATH="/opt/mine:$PATH"`+"\n"); err != nil {
		t.Fatal(err)
	}
	h.cd(t, h.plain)
	if got := h.path(); got != "/opt/mine:/usr/bin:/bin" {
		t.Fatalf("user PATH edit lost on revert: %q", got)
	}
}

func TestToolsDisabledByConfig(t *testing.T) {
	h := newToolsHarness(t)
	if err := runEnvScript(t.Context(), h.runner, "GISH_TOOLS=off\n"); err != nil {
		t.Fatal(err)
	}
	h.cd(t, h.pinned)
	if h.path() != "/usr/bin:/bin" {
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
