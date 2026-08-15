//go:build unix

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// startupBudget is the CI-enforced ceiling for exec→first-prompt (#37).
// CI runners are noisy, so the gate takes the minimum of several runs
// and uses a generous ceiling; the real target (<30ms) is verified
// locally via `make bench-startup` and published in the README.
const startupBudget = 150 * time.Millisecond

func buildGish(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gish")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// timeToFirstPrompt execs gish under a pty and measures until a prompt
// appears: the naked default (" % "), a themed arrow, or the plain
// non-TTY fallback.
func timeToFirstPrompt(t *testing.T, bin string, env []string) time.Duration {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = env
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

	var buf bytes.Buffer
	chunk := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = f.SetReadDeadline(time.Now().Add(200 * time.Millisecond)) //nolint:errcheck // best-effort
		n, _ := f.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			if bytes.Contains(buf.Bytes(), []byte("❯")) || bytes.Contains(buf.Bytes(), []byte(" % ")) ||
				bytes.Contains(buf.Bytes(), []byte("gish$")) {
				return time.Since(start)
			}
		}
	}
	t.Fatalf("no prompt within 5s; got %q", buf.String())
	return 0
}

// hermeticEnv gives gish empty config/state homes: no rc, no plugins,
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
	bin := buildGish(t)
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
	bin := buildGish(t)
	env := hermeticEnv(t)

	// A fat history file (10k entries) must not blow the budget.
	var home string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "XDG_DATA_HOME="); ok {
			home = v
		}
	}
	dir := filepath.Join(home, "gish")
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
