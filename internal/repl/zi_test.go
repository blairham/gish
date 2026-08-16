package repl

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

// ziHome points the manager at a temp root and preinstalls a plugin so
// tests are hermetic — no network, no git.
func ziHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ZI_GO_HOME", home)
	return home
}

func preinstallPlugin(t *testing.T, home, name, mainFile, content string) {
	t.Helper()
	dir := filepath.Join(home, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, mainFile), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func ziRunner(t *testing.T, out io.Writer) *interp.Runner {
	t.Helper()
	runner, err := interp.New(
		interp.StdIO(nil, out, out),
		interp.CallHandler(ziCallHandler(passthroughCall)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func run(t *testing.T, runner *interp.Runner, line string) error {
	t.Helper()
	return runner.Run(context.Background(), parseLine(t, line))
}

func TestZiLoadSourcesIntoLiveShell(t *testing.T) {
	home := ziHome(t)
	preinstallPlugin(t, home, "myplug", "myplug.plugin.zsh", "MYVAR=from_plugin\n")

	var out strings.Builder
	runner := ziRunner(t, &out)
	if err := run(t, runner, "zi load myplug"); err != nil {
		t.Fatalf("zi load: %v (out=%s)", err, out.String())
	}
	// The payload ran in the session: the variable persists.
	if got := shellVar(runner, "MYVAR", ""); got != "from_plugin" {
		t.Errorf("MYVAR = %q, want from_plugin", got)
	}
	if got := shellVar(runner, "ZI_GO_LOADED", ""); !strings.Contains(got, "myplug") {
		t.Errorf("ZI_GO_LOADED = %q", got)
	}
}

func TestZiIceAsProgramExtendsPath(t *testing.T) {
	home := ziHome(t)
	preinstallPlugin(t, home, "mytool", "mytool", "#!/bin/sh\necho tool-ran\n")

	var out strings.Builder
	runner := ziRunner(t, &out)
	if err := run(t, runner, `zi ice as"program"; zi load mytool`); err != nil {
		t.Fatalf("zi load: %v (out=%s)", err, out.String())
	}
	toolDir := filepath.Join(home, "plugins", "mytool")
	if got := shellVar(runner, "PATH", ""); !strings.Contains(got, toolDir) {
		t.Errorf("PATH = %q, missing %q", got, toolDir)
	}
	// And the tool actually runs through the session PATH. Windows
	// cannot exec a shebang script; the PATH extension above is the
	// portable half, execution is asserted where sh exists.
	if runtime.GOOS == "windows" {
		return
	}
	if err := run(t, runner, "mytool"); err != nil {
		t.Fatalf("running installed tool: %v", err)
	}
	if !strings.Contains(out.String(), "tool-ran") {
		t.Errorf("output = %q", out.String())
	}
}

func TestZiIceAppliesToNextLoadOnly(t *testing.T) {
	home := ziHome(t)
	preinstallPlugin(t, home, "one", "one.plugin.zsh", "ONE=1\n")
	preinstallPlugin(t, home, "two", "two.plugin.zsh", "TWO=1\n")

	var out strings.Builder
	runner := ziRunner(t, &out)
	// pick targets only the first load; the second resolves normally.
	if err := run(t, runner, `zi ice pick"one.plugin.zsh"; zi load one; zi load two`); err != nil {
		t.Fatalf("loads: %v (out=%s)", err, out.String())
	}
	if shellVar(runner, "ONE", "") != "1" || shellVar(runner, "TWO", "") != "1" {
		t.Errorf("ONE=%q TWO=%q", shellVar(runner, "ONE", ""), shellVar(runner, "TWO", ""))
	}
}

func TestZiListAndDelete(t *testing.T) {
	home := ziHome(t)
	preinstallPlugin(t, home, "listed", "listed.plugin.zsh", "true\n")

	var out strings.Builder
	runner := ziRunner(t, &out)
	if err := run(t, runner, "zi list"); err != nil {
		t.Fatalf("zi list: %v", err)
	}
	if !strings.Contains(out.String(), "listed") {
		t.Errorf("list output = %q", out.String())
	}

	if err := run(t, runner, "zi delete listed"); err != nil {
		t.Fatalf("zi delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "plugins", "listed")); !os.IsNotExist(err) {
		t.Error("plugin dir still exists after delete")
	}
}

func TestZiUnknownCommandFails(t *testing.T) {
	ziHome(t)
	var out strings.Builder
	runner := ziRunner(t, &out)
	err := run(t, runner, "zi frobnicate")
	status, ok := errors.AsType[interp.ExitStatus](err)
	if !ok || status != 1 {
		t.Fatalf("err = %v, want exit status 1", err)
	}
	if !strings.Contains(out.String(), "unknown command") {
		t.Errorf("stderr = %q", out.String())
	}
}

func TestZiLoadMissingPluginFails(t *testing.T) {
	ziHome(t)
	var out strings.Builder
	runner := ziRunner(t, &out)
	// Bare name, not installed, no URL to fetch from: must fail cleanly.
	err := run(t, runner, "zi load nonexistent")
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("stderr = %q", out.String())
	}
}
