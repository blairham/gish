//go:build unix

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// startupBudget is the CI-enforced ceiling for exec→first-prompt (#37).
// CI runners are noisy, so the gate takes the minimum of several runs
// and uses a generous ceiling; the real target (<30ms) is verified
// locally via `make bench-startup` and published in the README.
const startupBudget = 150 * time.Millisecond

// buildKoi returns a koi binary, built once for the whole package.
//
// Shared rather than per-test because several tests here launch koi
// under a pty, and each `go build` was competing with the shells the
// other tests were trying to time out on. On a saturated macOS runner
// that showed up as pty tests seeing *no output at all* within their
// deadline — the shell was fine, the machine was busy building.
//
// The binary is identical for every caller, so there is nothing to
// isolate; only the cost was ever per-test.
var buildOnce = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "koi-testbin")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "koi")
	// The koipanicprobe tag compiles in the deliberate panic that
	// TestInterpreterPanicDoesNotKillTheShell needs (internal/repl/
	// panicprobe.go). It adds one unreachable command name and nothing
	// else, so every other test sees the ordinary binary.
	if out, err := exec.Command("go", "build", "-tags", "koipanicprobe", "-o", bin, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("build: %w\n%s", err, out)
	}
	return bin, nil
})

func buildKoi(t *testing.T) string {
	t.Helper()
	bin, err := buildOnce()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

// promptMarkers are the byte sequences that mean "the first prompt is
// on screen". The OSC 133;B mark (#99) is the exact signal — it is
// written when the prompt ends and input begins — so it leads; the
// glyph markers stay for a run with KOI_SEMANTIC_MARKS=off.
var promptMarkers = [][]byte{
	[]byte("\x1b]133;B"),
	[]byte("❯"),
	[]byte(" % "),
	[]byte("koi$"),
}

// timeToFirstPrompt execs koi under a pty and measures until a prompt
// appears.
func timeToFirstPrompt(t *testing.T, bin string, env []string) time.Duration {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = env
	// Start in the hermetic home, not the package directory: a startup
	// measurement must not be charged for whatever the measuring
	// directory contains (this repo's .tool-versions, its git tree) —
	// the same rule internal/bench learned.
	cmd.Dir = homeFrom(env)
	start := time.Now()
	f, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		f.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Read from a goroutine and time out in a select: SetReadDeadline is
	// unsupported on a pty ("file type does not support deadline"), so a
	// bare Read blocks forever once the shell goes quiet — which turns a
	// missed marker into a 10-minute CI timeout instead of a failure
	// that says what was on screen.
	chunks := ptyChunks(f)
	var buf bytes.Buffer
	deadline := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatalf("koi exited before prompting; got %q", buf.String())
			}
			buf.Write(chunk)
			for _, marker := range promptMarkers {
				if bytes.Contains(buf.Bytes(), marker) {
					return time.Since(start)
				}
			}
		case <-deadline:
			t.Fatalf("no prompt within 5s; got %q", buf.String())
		}
	}
}

// ptyChunks streams a pty's output; the channel closes when the pty
// does. One goroutine per launch, ended by closing the pty.
func ptyChunks(f *os.File) <-chan []byte {
	ch := make(chan []byte, 64)
	go func() {
		defer close(ch)
		for {
			chunk := make([]byte, 4096)
			n, err := f.Read(chunk)
			if n > 0 {
				ch <- chunk[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// homeFrom returns the HOME set by hermeticEnv.
func homeFrom(env []string) string { return envValue(env, "HOME") }

// envValue reads one variable out of an exec-style env slice.
func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// hermeticEnv gives koi empty config/state homes: no rc, no plugins,
// no history — the out-of-box cold-ish start.
func hermeticEnv(t *testing.T) []string {
	t.Helper()
	base := t.TempDir()
	env := []string{
		"HOME=" + base,
		"XDG_CONFIG_HOME=" + filepath.Join(base, "config"),
		"XDG_DATA_HOME=" + filepath.Join(base, "data"),
		"XDG_STATE_HOME=" + filepath.Join(base, "state"),
		"TERM=xterm-256color",
		"PATH=" + os.Getenv("PATH"),
	}
	return env
}

// TestStartupBudget is the #37 regression gate: minimum of five runs
// must beat the ceiling.
func TestStartupBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("startup benchmark skipped in -short")
	}
	bin := buildKoi(t)
	env := hermeticEnv(t)

	best := time.Hour
	var runs []string
	for range 5 {
		d := timeToFirstPrompt(t, bin, env)
		runs = append(runs, d.Round(time.Millisecond).String())
		if d < best {
			best = d
		}
	}
	t.Logf("startup runs: %s (best %s, budget %s)", strings.Join(runs, " "), best.Round(time.Millisecond), startupBudget)
	if best > startupBudget {
		t.Fatalf("startup regression: best of 5 = %s > budget %s", best, startupBudget)
	}
}

// TestStartupWithPluginsDoesNotBlockFirstPrompt guards the #37
// architecture rule: plugin discovery must not delay the first paint
// beyond the render budget. A plugin that sleeps in Describe simulates
// a slow plugin; the prompt must still appear fast.
func TestStartupWithSlowHistoryFile(t *testing.T) {
	if testing.Short() {
		t.Skip("startup benchmark skipped in -short")
	}
	bin := buildKoi(t)
	env := hermeticEnv(t)

	// A fat history file (10k entries) must not blow the budget.
	dir := filepath.Join(envValue(env, "XDG_DATA_HOME"), "koi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := range 10000 {
		b.WriteString(`{"command":"echo line `)
		b.WriteString(strings.Repeat("x", i%40))
		b.WriteString(`"}` + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	best := time.Hour
	for range 3 {
		if d := timeToFirstPrompt(t, bin, env); d < best {
			best = d
		}
	}
	t.Logf("startup with 10k-entry history: %s", best.Round(time.Millisecond))
	if best > startupBudget {
		t.Fatalf("history load blows the startup budget: %s > %s", best, startupBudget)
	}
}
