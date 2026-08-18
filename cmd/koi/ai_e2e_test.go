//go:build darwin || linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/creack/pty"
)

// ansiRe strips escape sequences: syntax highlighting styles the
// rendered buffer, so assertions must compare plain text.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// TestComposePrefixEndToEnd drives the real ?? flow under a pty against
// the fixture AI provider: the query goes to the plugin, the candidate
// lands in the next editor buffer wrapped in the sandbox invocation —
// and is NOT executed.
func TestComposePrefixEndToEnd(t *testing.T) {
	bin := buildKoi(t)
	base := t.TempDir()
	pluginDir := filepath.Join(base, "data", "koi", "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(pluginDir, "fixture"),
		"../../internal/pluginhost/testdata/fixture")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = []string{
		"HOME=" + base,
		"XDG_CONFIG_HOME=" + filepath.Join(base, "config"),
		"XDG_DATA_HOME=" + filepath.Join(base, "data"),
		"TERM=xterm-256color",
		"PATH=" + os.Getenv("PATH"),
		// This test asserts a plugin-backed feature, so it must not
		// lose the plugin to the launch deadline under a loaded run
		// (#189). The product default stays 2s.
		"KOI_PLUGIN_DESCRIBE_TIMEOUT=60s",
	}
	cmd.Dir = base // a quiet cwd: no repo pins, no tools notices
	// Wide pty: the preloaded buffer must not wrap mid-assertion.
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 200})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		f.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// A reader goroutine feeds chunks over a channel: pty reads can
	// block past any read deadline once output goes quiet, so the
	// timeout lives in the select, not the Read.
	chunks := make(chan []byte, 64)
	go func() {
		for {
			chunk := make([]byte, 4096)
			n, err := f.Read(chunk)
			if n > 0 {
				chunks <- chunk[:n]
			}
			if err != nil {
				close(chunks)
				return
			}
		}
	}()

	var buf bytes.Buffer
	waitFor := func(want string) []byte { return aiWaitFor(t, chunks, &buf, want) }

	waitFor(" % ") // the naked first prompt
	if _, err := f.WriteString("?? list files\r"); err != nil {
		t.Fatal(err)
	}
	// The candidate arrives in the buffer, sandbox-wrapped, with the
	// provider's rationale above it — and nothing has executed.
	got := waitFor("sandbox --profile workspace -- echo composed:list files")
	if !bytes.Contains(got, []byte("fixture rationale")) {
		t.Errorf("rationale not shown:\n%q", got)
	}
	// Had the composed command executed, its output would start a line
	// of its own; in the buffer it only ever appears after "echo ".
	if regexp.MustCompile(`(?m)^composed:list files`).Match(got) {
		t.Errorf("composed command appears to have executed:\n%q", got)
	}
}

// TestAgentEndToEnd drives the full plan → approve → gated-execute flow
// under a pty against the fixture provider.
func TestAgentEndToEnd(t *testing.T) {
	bin := buildKoi(t)
	base := t.TempDir()
	pluginDir := filepath.Join(base, "data", "koi", "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(pluginDir, "fixture"),
		"../../internal/pluginhost/testdata/fixture")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = []string{
		"HOME=" + base,
		"XDG_CONFIG_HOME=" + filepath.Join(base, "config"),
		"XDG_DATA_HOME=" + filepath.Join(base, "data"),
		"TERM=xterm-256color",
		"PATH=" + os.Getenv("PATH"),
		// This test asserts a plugin-backed feature, so it must not
		// lose the plugin to the launch deadline under a loaded run
		// (#189). The product default stays 2s.
		"KOI_PLUGIN_DESCRIBE_TIMEOUT=60s",
	}
	cmd.Dir = base
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 200})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		f.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	chunks := make(chan []byte, 64)
	go func() {
		for {
			chunk := make([]byte, 4096)
			n, rerr := f.Read(chunk)
			if n > 0 {
				chunks <- chunk[:n]
			}
			if rerr != nil {
				close(chunks)
				return
			}
		}
	}()
	var buf bytes.Buffer
	waitFor := func(want string) []byte { return aiWaitFor(t, chunks, &buf, want) }

	waitFor(" % ")
	if _, err := f.WriteString("agent \"do the thing\"\r"); err != nil {
		t.Fatal(err)
	}
	// The plan renders first — nothing has executed yet. The gate is a
	// huh select on a real terminal: Enter picks the focused option.
	got := waitFor("run this plan?")
	if !bytes.Contains(got, []byte("fixture plan for: do the thing")) {
		t.Errorf("plan summary missing:\n%q", got)
	}
	// The plan listing shows "$ echo agent-step-one"; actual execution
	// would print the word at the start of its own line.
	if regexp.MustCompile(`(?m)^agent-step-one`).Match(got) {
		t.Errorf("step output before approval:\n%q", got)
	}
	if _, err := f.WriteString("\r"); err != nil { // select "run all"; destructive still gates
		t.Fatal(err)
	}
	waitFor("agent-step-one")                      // step 1 actually executed
	waitFor("run step 2 (destructive)?")           // step 2 gates individually even in all mode
	if _, err := f.WriteString("\r"); err != nil { // select "run (sandboxed)"
		t.Fatal(err)
	}
	waitFor("plan complete (2 step(s))")

	// The plan is an artifact with outcomes recorded.
	entries, err := os.ReadDir(filepath.Join(base, "data", "koi", "agent"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no plan artifact: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(base, "data", "koi", "agent", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("- step 1: ok")) || !bytes.Contains(data, []byte("- step 2: ok")) {
		t.Errorf("artifact outcomes missing:\n%s", data)
	}
}

// aiWaitFor reads the pty until want appears, and returns the output
// with escapes stripped.
//
// It exists once rather than as a closure per test because it was a
// closure per test, byte for byte, and ptyharness_test.go records what
// that costs: "a pattern that will be fixed in one of them". The
// silence-versus-elapsed rule here is precisely such a fix, so it is
// kept somewhere it cannot be applied to only half the callers.
//
// Both bounds are the harness's own (#189): give up when the terminal
// goes quiet, not when a loaded runner has simply taken a while, with
// the total only as a livelock backstop.
func aiWaitFor(t *testing.T, chunks <-chan []byte, buf *bytes.Buffer, want string) []byte {
	t.Helper()
	hard := time.After(e2eBudget)
	idle := time.NewTimer(e2eIdleBudget)
	defer idle.Stop()
	for {
		plain := ansiRe.ReplaceAll(buf.Bytes(), nil)
		if bytes.Contains(plain, []byte(want)) {
			return plain
		}
		// The one failure that can never resolve by waiting: the host
		// gave up on the fixture plugin, so the thing being waited for
		// is not coming. Say that instead of timing out with it buried
		// in kilobytes of redraw, which is how this cost an afternoon.
		if !bytes.Contains([]byte(want), []byte(providerAbsent)) &&
			bytes.Contains(plain, []byte(providerAbsent)) {
			t.Fatalf("the fixture plugin never loaded, so %q can never appear — "+
				"the host reported %q (raise KOI_PLUGIN_DESCRIBE_TIMEOUT if this is a slow machine)",
				want, providerAbsent)
		}
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatalf("pty closed before %q; got %q", want, buf.String())
			}
			buf.Write(chunk)
			idle.Reset(e2eIdleBudget)
		case <-idle.C:
			t.Fatalf("no output for %s while waiting for %q; got %q", e2eIdleBudget, want, buf.String())
		case <-hard:
			t.Fatalf("did not see %q within %s; got %q", want, e2eBudget, buf.String())
		}
	}
}

// providerAbsent is what the shell prints when no AIProvider plugin
// answered in time. Matching on it is deliberate coupling: it is the
// difference between "this feature is broken" and "the harness lost the
// plugin", and only one of those is worth anyone's morning.
const providerAbsent = "no AI provider plugin installed"
