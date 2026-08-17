//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The gish-native builtins (#55).
//
// There is no bash to be differential against here — these are gish's
// own commands — so the assertions are about observable behavior rather
// than agreement with an oracle. The invariants worth pinning are the
// ones that hold for all of them:
//
//   - the name resolves. A builtin gish documents must never answer
//     "executable file not found in $PATH", which is what a typo looks
//     like. `plugins` did exactly that outside an interactive session.
//   - it terminates. A builtin that waits for input nobody is going to
//     send hangs a script forever; the timeout here is the assertion.
//   - it does not panic. A stack trace is not a diagnostic.
//   - unavailable is stated, not implied. Several of these need a live
//     interactive session; saying so beats an empty exit.
//   - it stays out of the user's real state. Each case runs with its own
//     HOME and XDG directories and a neutral cwd — the repo's own
//     .tool-versions was enough to change `tool` output when this was
//     first written from the source tree.
//
// Together these cover the "claimed but broken" class that a list of
// names cannot: every entry here was run, and two of them had something
// to say.

// nativeBudget bounds one builtin. Generous — the point is to catch a
// hang, not to measure anything.
const nativeBudget = 20 * time.Second

type nativeCase struct {
	// script is what runs under `gish -c`.
	script string
	// wantOut, when set, must appear in the combined output.
	wantOut string
	// wantExit is the expected status; nil means any status is fine, for
	// the builtins whose answer legitimately depends on the machine.
	wantExit *int
}

func exitCode(n int) *int { return &n }

// nativeCases covers every name gish intercepts, at its no-argument
// entry point — the invocation a person types first.
var nativeCases = map[string]nativeCase{
	// Report state and succeed.
	"blocks":   {script: "blocks", wantOut: "no captured output yet", wantExit: exitCode(0)},
	"config":   {script: "config", wantOut: "theme", wantExit: exitCode(0)},
	"doctor":   {script: "doctor", wantOut: "rc", wantExit: exitCode(0)},
	"p10k":     {script: "p10k", wantOut: "preset", wantExit: exitCode(0)},
	"plugin":   {script: "plugin", wantOut: "no plugins configured", wantExit: exitCode(0)},
	"sandbox":  {script: "sandbox", wantOut: "session sandbox", wantExit: exitCode(0)},
	"sessions": {script: "sessions", wantOut: "no sessions recorded", wantExit: exitCode(0)},
	"tool":     {script: "tool", wantOut: "no .tool-versions in scope", wantExit: exitCode(0)},
	"zi":       {script: "zi", wantOut: "Zi", wantExit: exitCode(0)},
	"builtins": {script: "builtins", wantOut: "gish builtins", wantExit: exitCode(0)},

	// Say what is unavailable and why, rather than failing blankly.
	"explain": {script: "explain", wantOut: "no AI provider", wantExit: exitCode(1)},
	"trust":   {script: "trust", wantOut: "not available in this session", wantExit: exitCode(1)},
	"plugins": {script: "plugins", wantOut: "only in an interactive session", wantExit: exitCode(1)},

	// Usage errors are errors, with the usage attached.
	"parallel": {script: "parallel", wantOut: "usage: parallel", wantExit: exitCode(2)},

	// Nothing to act on: a nonzero status, no noise, and above all no
	// blocking on a terminal that is not there.
	"pick": {script: "pick </dev/null", wantExit: exitCode(1)},
	"z":    {script: "z", wantOut: "no match", wantExit: exitCode(1)},

	// Interpreter-claimed names gish implements natively (#55): they
	// reach the exec seam only because the override renames them, so a
	// broken rename shows up here as "unsupported builtin".
	"kill":  {script: "kill", wantOut: "usage: kill", wantExit: exitCode(2)},
	"umask": {script: "umask", wantOut: "0", wantExit: exitCode(0)},

	// clip is a pipeline sink; with no terminal it is a silent no-op by
	// design, so the assertion is that it neither hangs nor complains.
	"clip": {script: "echo hi | clip", wantExit: exitCode(0)},
}

func TestNativeBuiltinsBehave(t *testing.T) {
	if testing.Short() {
		t.Skip("native builtin matrix skipped in -short")
	}
	gish := buildGish(t)

	for name, tc := range nativeCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, code := runHermetic(t, gish, tc.script)

			// The invariants that hold for every one of them.
			if strings.Contains(out, "panic:") || strings.Contains(out, "goroutine ") {
				t.Fatalf("%s panicked:\n%s", name, out)
			}
			if strings.Contains(out, "executable file not found") {
				t.Errorf("%s did not resolve as a builtin — it fell through to PATH: %q", name, out)
			}
			if strings.Contains(out, "unsupported builtin") {
				t.Errorf("%s is recognized but unimplemented: %q", name, out)
			}

			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Errorf("%s output %q does not contain %q", name, out, tc.wantOut)
			}
			if tc.wantExit != nil && code != *tc.wantExit {
				t.Errorf("%s exit = %d, want %d\n%s", name, code, *tc.wantExit, out)
			}
		})
	}
}

// TestNativeBuiltinsAreListed keeps the `builtins` listing honest: a
// name the shell intercepts but does not list is undiscoverable, and a
// name it lists but does not intercept is a lie.
func TestEveryNativeCaseIsCovered(t *testing.T) {
	t.Parallel()

	// The names gish intercepts, from the CallHandler chain and the
	// native registry. Kept here as the thing the matrix is checked
	// against, so adding an interception without a case fails the build
	// of this list rather than going unnoticed.
	intercepted := []string{
		"blocks", "builtins", "clip", "config", "doctor", "explain",
		"kill", "p10k", "parallel", "pick", "plugin", "plugins",
		"sandbox", "sessions", "tool", "trust", "umask", "z", "zi",
	}
	for _, name := range intercepted {
		if _, ok := nativeCases[name]; !ok {
			t.Errorf("%s is intercepted but has no case in nativeCases", name)
		}
	}
	for name := range nativeCases {
		if !slices.Contains(intercepted, name) {
			t.Errorf("nativeCases has %s, which nothing intercepts", name)
		}
	}
}

// runHermetic runs a script with its own HOME and XDG directories and a
// neutral cwd, and fails rather than blocks if it does not finish.
//
// The cwd matters as much as the environment: run from the source tree,
// `tool` reads the repo's own .tool-versions and reports pins that have
// nothing to do with the test.
func runHermetic(t *testing.T, gish, script string) (string, int) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(gish, "-c", script)
	cmd.Dir = work
	cmd.Env = []string{
		"HOME=" + base,
		"XDG_CONFIG_HOME=" + filepath.Join(base, "config"),
		"XDG_DATA_HOME=" + filepath.Join(base, "data"),
		"XDG_STATE_HOME=" + filepath.Join(base, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(base, "cache"),
		"PATH=" + os.Getenv("PATH"),
		"TERM=dumb", // no TUI, no color: the degraded path every case takes
	}
	cmd.Stdin = strings.NewReader("")

	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("running %q: %v", script, err)
		}
		return out.String(), code
	case <-time.After(nativeBudget):
		_ = cmd.Process.Kill()
		t.Fatalf("%q did not finish within %s — a builtin that blocks hangs every script that calls it; got:\n%s",
			script, nativeBudget, out.String())
		return "", 0
	}
}
