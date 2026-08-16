package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// toolEnv pins every location the tool command touches into temp dirs.
func toolEnv(t *testing.T) (workDir string) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "proj")
	if err := os.MkdirAll(filepath.Join(base, "asdf", "installs", "golang", "1.26.6", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASDF_DATA_DIR", filepath.Join(base, "asdf"))
	t.Setenv("MISE_DATA_DIR", filepath.Join(base, "mise"))
	t.Setenv("HOME", base)
	return work
}

func TestToolPinAndOverview(t *testing.T) {
	work := toolEnv(t)
	rc := filepath.Join(t.TempDir(), "gishrc")
	out, _, err := runConfigScript(t, rc,
		"cd "+quoteArg(t, work)+"\ntool pin golang 1.26.6\ntool pin nodejs 22.0.0\ntool\ntool list golang\n")
	if err != nil {
		t.Fatal(err)
	}
	data, rerr := os.ReadFile(filepath.Join(work, ".tool-versions"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got := string(data); got != "golang 1.26.6\nnodejs 22.0.0\n" {
		t.Errorf("pins file = %q", got)
	}
	// The uninstalled pin is called out at pin time and in the overview.
	if !strings.Contains(out, "note: nodejs 22.0.0 is not installed") {
		t.Errorf("no uninstalled note: %q", out)
	}
	if !strings.Contains(out, "NOT INSTALLED (tool install nodejs 22.0.0)") {
		t.Errorf("overview missing the gap: %q", out)
	}
	if !strings.Contains(out, "golang       1.26.6") {
		t.Errorf("overview missing resolved pin: %q", out)
	}
	if !strings.Contains(out, "* 1.26.6") {
		t.Errorf("list missing active marker: %q", out)
	}
}

func TestToolGlobalWritesHomeFile(t *testing.T) {
	work := toolEnv(t)
	rc := filepath.Join(t.TempDir(), "gishrc")
	if _, _, err := runConfigScript(t, rc, "cd "+quoteArg(t, work)+"\ntool global golang 1.26.6\n"); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".tool-versions"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "golang 1.26.6\n" {
		t.Errorf("global pins = %q", got)
	}
}

func TestToolInstallWithoutAsdf(t *testing.T) {
	toolEnv(t)
	t.Setenv("PATH", t.TempDir()) // no asdf here
	rc := filepath.Join(t.TempDir(), "gishrc")
	_, errOut, _ := runConfigScript(t, rc, "tool install golang 1.26.6\n")
	if !strings.Contains(errOut, "asdf is not installed") || !strings.Contains(errOut, "--from") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestToolInstallBadFrom(t *testing.T) {
	toolEnv(t)
	rc := filepath.Join(t.TempDir(), "gishrc")
	_, errOut, _ := runConfigScript(t, rc, "tool install thing 1.0.0 --from not-a-repo\n")
	if !strings.Contains(errOut, "owner/repo") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestToolUnknownArgs(t *testing.T) {
	toolEnv(t)
	rc := filepath.Join(t.TempDir(), "gishrc")
	_, errOut, _ := runConfigScript(t, rc, "tool frobnicate\n")
	if !strings.Contains(errOut, "unknown arguments") {
		t.Errorf("stderr = %q", errOut)
	}
}
