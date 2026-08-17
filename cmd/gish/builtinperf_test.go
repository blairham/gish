//go:build unix

package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Builtin dispatch performance (#55).
//
// Builtins run in loops, and every one of them now walks a CallHandler
// chain that grew a link when printf was taken over. The cost of that
// link is paid by every command in the shell, not just printf, which is
// the kind of change that is easy to make and hard to notice.
//
// Shaped like the #37 startup gate rather than a micro-benchmark: best
// of several runs against a deliberately generous ceiling. docs/bench.md
// is explicit that timing must never be a CI gate that a loaded runner
// can fail, so this is sized to catch an order-of-magnitude regression —
// a chain walked per character instead of per command — and nothing
// finer. The honest per-call numbers come from `go test -bench` in
// internal/builtins.

// builtinLoopBudget bounds 500 builtin invocations. Locally this runs in
// well under a tenth of it; the headroom is for CI, not for us.
const builtinLoopBudget = 4 * time.Second

// builtinLoopRuns is how many times the script runs; the best is scored,
// because a shared runner's worst tells you about the runner.
const builtinLoopRuns = 3

func timeScript(t *testing.T, bin, script string) time.Duration {
	t.Helper()
	best := time.Hour
	for range builtinLoopRuns {
		cmd := exec.Command(bin, "-c", script)
		cmd.Env = hermeticEnv(t)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		d := time.Since(start)
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		if d < best {
			best = d
		}
	}
	return best
}

// TestBuiltinDispatchStaysCheap guards the shared cost of the
// CallHandler chain.
func TestBuiltinDispatchStaysCheap(t *testing.T) {
	if testing.Short() {
		t.Skip("perf guard skipped in -short")
	}
	bin := buildGish(t)

	for _, tc := range []struct{ name, script string }{
		// printf is the one that now runs gish's own implementation.
		{"printf", `i=0; while [ $i -lt 500 ]; do printf '%s-%d\n' x $i >/dev/null; i=$((i+1)); done; echo done`},
		// A builtin that does *not* match, so it pays only the chain walk.
		// If interception ever starts costing real time, this moves too.
		{"echo", `i=0; while [ $i -lt 500 ]; do echo x >/dev/null; i=$((i+1)); done; echo done`},
		{"test", `i=0; while [ $i -lt 500 ]; do [ "$i" -lt 500 ] || exit 1; i=$((i+1)); done; echo done`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			best := timeScript(t, bin, tc.script)
			t.Logf("500 %s calls: %s (budget %s)", tc.name, best.Round(time.Millisecond), builtinLoopBudget)
			if best > builtinLoopBudget {
				t.Errorf("500 %s calls took %s, over the %s budget", tc.name, best, builtinLoopBudget)
			}
		})
	}
}

// TestPrintfInterceptionDoesNotShadowOtherCommands is the correctness
// half of the same worry: a CallHandler that matches too eagerly would
// break commands whose names merely start with, or contain, printf.
func TestPrintfInterceptionIsExact(t *testing.T) {
	if testing.Short() {
		t.Skip("pty-free e2e skipped in -short")
	}
	bin := buildGish(t)
	out, code := runShell(t, bin, `printf_like() { echo "function ran"; }; printf_like; myprintf() { echo "suffix ran"; }; myprintf`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"function ran", "suffix ran"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q — interception matched too broadly", out, want)
		}
	}
}
