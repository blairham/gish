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
	bin := buildGish(t)
	base := t.TempDir()
	pluginDir := filepath.Join(base, "data", "gish", "plugins")
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
	waitFor := func(want string) []byte {
		t.Helper()
		deadline := time.After(15 * time.Second)
		for {
			plain := ansiRe.ReplaceAll(buf.Bytes(), nil)
			if bytes.Contains(plain, []byte(want)) {
				return plain
			}
			select {
			case chunk, ok := <-chunks:
				if !ok {
					t.Fatalf("pty closed before %q; got %q", want, buf.String())
				}
				buf.Write(chunk)
			case <-deadline:
				t.Fatalf("did not see %q within 15s; got %q", want, buf.String())
			}
		}
	}

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
	bin := buildGish(t)
	base := t.TempDir()
	pluginDir := filepath.Join(base, "data", "gish", "plugins")
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
	waitFor := func(want string) []byte {
		t.Helper()
		deadline := time.After(15 * time.Second)
		for {
			plain := ansiRe.ReplaceAll(buf.Bytes(), nil)
			if bytes.Contains(plain, []byte(want)) {
				return plain
			}
			select {
			case chunk, ok := <-chunks:
				if !ok {
					t.Fatalf("pty closed before %q; got %q", want, buf.String())
				}
				buf.Write(chunk)
			case <-deadline:
				t.Fatalf("did not see %q within 15s; got %q", want, buf.String())
			}
		}
	}

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
	entries, err := os.ReadDir(filepath.Join(base, "data", "gish", "agent"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no plan artifact: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(base, "data", "gish", "agent", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("- step 1: ok")) || !bytes.Contains(data, []byte("- step 2: ok")) {
		t.Errorf("artifact outcomes missing:\n%s", data)
	}
}
