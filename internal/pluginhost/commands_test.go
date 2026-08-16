package pluginhost_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/pluginhost"
)

// newIndex builds a command index over the fixture, waiting for the
// async interrogation to land.
func newIndex(t *testing.T, reserved func(string) bool) (*pluginhost.Host, *pluginhost.CommandIndex) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // hermetic cache
	h := newHost(t)
	ci := h.NewCommandIndex(reserved)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if cmds := ci.CommandsOf(fixtureName()); len(cmds) > 0 {
			// The index is populated before its cache is written, so the
			// test must not race the detached save against t.TempDir()
			// cleanup — which is exactly how this went flaky on Windows.
			t.Cleanup(ci.Wait)
			return h, ci
		}
		if time.Now().After(deadline) {
			t.Fatal("command index never populated")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func cmdRunner(t *testing.T, ci *pluginhost.CommandIndex, out *strings.Builder) *interp.Runner {
	t.Helper()
	runner, err := interp.New(
		interp.StdIO(nil, out, out),
		interp.ExecHandlers(ci.ExecMiddleware),
	)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func runCmd(t *testing.T, runner *interp.Runner, line string) error {
	t.Helper()
	file, err := syntax.NewParser().Parse(strings.NewReader(line), "test")
	if err != nil {
		t.Fatal(err)
	}
	return runner.Run(context.Background(), file)
}

func TestPluginCommandRuns(t *testing.T) {
	_, ci := newIndex(t, nil)
	var out strings.Builder
	runner := cmdRunner(t, ci, &out)
	if err := runCmd(t, runner, "fixture-echo one two"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "echo:one,two") {
		t.Errorf("output = %q", out.String())
	}
}

func TestPluginCommandExitStatus(t *testing.T) {
	_, ci := newIndex(t, nil)
	var out strings.Builder
	runner := cmdRunner(t, ci, &out)
	err := runCmd(t, runner, "fixture-fail")
	status, ok := errors.AsType[interp.ExitStatus](err)
	if !ok || status != 3 {
		t.Fatalf("err = %v, want exit 3", err)
	}
	if !strings.Contains(out.String(), "deliberate failure") {
		t.Errorf("stderr = %q", out.String())
	}
}

func TestPluginCommandStdin(t *testing.T) {
	_, ci := newIndex(t, nil)
	var out strings.Builder
	runner := cmdRunner(t, ci, &out)
	if err := runCmd(t, runner, "echo hello | fixture-upper"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "HELLO") {
		t.Errorf("output = %q", out.String())
	}
}

func TestReservedNameRejected(t *testing.T) {
	reserved := func(name string) bool { return name == "cd" }
	_, ci := newIndex(t, reserved)
	if cmds := ci.CommandsOf(fixtureName()); len(cmds) != 3 {
		t.Errorf("commands = %v, want the reserved cd claim dropped", cmds)
	}
	for _, c := range ci.CommandsOf(fixtureName()) {
		if c == "cd" {
			t.Error("reserved name registered")
		}
	}
}

func TestIndexCacheWarmStart(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	h := pluginhost.NewHost(pluginDir(t), pluginhost.WithBackoff(10*time.Millisecond))
	if err := h.Discover(); err != nil {
		t.Fatal(err)
	}
	ci := h.NewCommandIndex(nil)
	deadline := time.Now().Add(10 * time.Second)
	for len(ci.CommandsOf(fixtureName())) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("index never populated")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.Close()

	// The cache file exists and a fresh index serves names immediately,
	// without waiting for interrogation.
	cachePath := filepath.Join(stateDir, "gish", "command-index.json")
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(cachePath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cache never written")
		}
		time.Sleep(20 * time.Millisecond)
	}

	h2 := pluginhost.NewHost(pluginDir(t), pluginhost.WithBackoff(10*time.Millisecond))
	if err := h2.Discover(); err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	ci2 := h2.NewCommandIndex(nil)
	if cmds := ci2.CommandsOf(fixtureName()); len(cmds) == 0 {
		t.Error("warm start did not serve cached command names")
	}
}
