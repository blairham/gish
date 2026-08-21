//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The koi-native builtins (#55).
//
// There is no bash to be differential against here — these are koi's
// own commands — so the assertions are about observable behavior rather
// than agreement with an oracle. The invariants worth pinning are the
// ones that hold for all of them:
//
//   - the name resolves. A builtin koi documents must never answer
//     "command not found", which is what a typo looks like. `plugins`
//     did exactly that outside an interactive session.
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
	// script is what runs under `koi -c`.
	script string
	// wantOut, when set, must appear in the combined output.
	wantOut string
	// wantExit is the expected status; nil means any status is fine, for
	// the builtins whose answer legitimately depends on the machine.
	wantExit *int
}

func exitCode(n int) *int { return &n }

// nativeCases covers every name koi intercepts, at its no-argument
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
	"builtins": {script: "builtins", wantOut: "koi builtins", wantExit: exitCode(0)},
	// help (#196) at its two entry points a switcher types first: the
	// bare overview, and one shell builtin explained.
	"help": {script: "help && help cd", wantOut: "change the working directory", wantExit: exitCode(0)},

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

	// Interpreter-claimed names koi implements natively (#55): they
	// reach the exec seam only because the override renames them, so a
	// broken rename shows up here as "unsupported builtin".
	"kill":  {script: "kill", wantOut: "usage: kill", wantExit: exitCode(2)},
	"umask": {script: "umask", wantOut: "0", wantExit: exitCode(0)},
	// times cannot be differential — the numbers are real elapsed time
	// and differ every run — so the assertion is the shape bash prints:
	// two lines of "0m0.000s 0m0.000s", shell then children.
	"times": {script: "times", wantOut: "m0", wantExit: exitCode(0)},
	// fc lists interactive history, and the script path records none. It
	// used to say so and exit 1; bash prints nothing and succeeds, and a
	// script listing its history defensively should not be ended by it
	// under `set -e` (#306). So the case is what bash does: seed the list
	// through `history -s`, since nothing else puts a command in it
	// without running it, and check the entry comes back.
	"fc": {script: "history -s seeded; fc -l", wantOut: "seeded", wantExit: exitCode(0)},
	// newgrp is deliberately not provided (#61). It is still claimed, so
	// the name explains itself and points at the system's own rather
	// than shadowing it with "unsupported builtin".
	"newgrp": {script: "newgrp", wantOut: "/usr/bin/newgrp", wantExit: exitCode(1)},

	// clip is a pipeline sink; with no terminal it is a silent no-op by
	// design, so the assertion is that it neither hangs nor complains.
	"clip": {script: "echo hi | clip", wantExit: exitCode(0)},
}

func TestNativeBuiltinsBehave(t *testing.T) {
	if testing.Short() {
		t.Skip("native builtin matrix skipped in -short")
	}
	koi := buildKoi(t)

	for name, tc := range nativeCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, code := runHermetic(t, koi, tc.script)

			// The invariants that hold for every one of them.
			if strings.Contains(out, "panic:") || strings.Contains(out, "goroutine ") {
				t.Fatalf("%s panicked:\n%s", name, out)
			}
			if strings.Contains(out, "command not found") {
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

// interceptedNative is the names koi intercepts, from the CallHandler
// chain and the native registry. Kept as one list so the matrices can be
// checked against it: adding an interception without a case fails here
// rather than going unnoticed, and cmd/koi/builtinloc_test.go reads the
// same list to demand a diagnostic-location case for each (#611).
var interceptedNative = []string{
	"blocks", "builtins", "clip", "config", "doctor", "explain",
	"fc", "help", "kill", "newgrp", "p10k", "parallel", "pick",
	"plugin", "plugins",
	"sandbox", "sessions", "times", "tool", "trust", "umask", "z",
	"zi",
}

// TestNativeBuiltinsAreListed keeps the `builtins` listing honest: a
// name the shell intercepts but does not list is undiscoverable, and a
// name it lists but does not intercept is a lie.
func TestEveryNativeCaseIsCovered(t *testing.T) {
	t.Parallel()

	for _, name := range interceptedNative {
		if _, ok := nativeCases[name]; !ok {
			t.Errorf("%s is intercepted but has no case in nativeCases", name)
		}
	}
	for name := range nativeCases {
		if !slices.Contains(interceptedNative, name) {
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
func runHermetic(t *testing.T, koi, script string) (string, int) {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(koi, "-c", script)
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

// TestTimesShape pins the format rather than the values: two lines,
// each two fields of the form 0m0.000s — shell times then children's.
// A builtin that printed one line, or seconds without the minutes, would
// still satisfy the substring check above.
func TestTimesShape(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	out, code := runHermetic(t, buildKoi(t), "times")
	if code != 0 {
		t.Fatalf("times exit = %d: %s", code, out)
	}
	shape := regexp.MustCompile(`^\d+m\d+\.\d{3}s \d+m\d+\.\d{3}s\n\d+m\d+\.\d{3}s \d+m\d+\.\d{3}s\n$`)
	if !shape.MatchString(out) {
		t.Errorf("times output %q does not match bash's two-line shape", out)
	}
}

// The shell syntax `help` answers for (#557). `help` is koi's own
// surface — bash's text for these is GPLv3 and koi's is written from
// scratch — so there is no oracle for the wording, and what can be
// asserted instead is the property that makes the listing worth having:
// every topic names a construct *this* shell runs.
//
// Each row is the topic as it is listed and a script that runs the
// construct and prints one marker. The marker is deliberately not a
// word the construct itself echoes back: `select w in ok` prints `1) ok`
// in its menu, so a containment check for "ok" would pass without the
// body ever running.
const syntaxMark = "syntax-ok"

var helpSyntaxProbes = map[string]string{
	"!":         "! false && echo " + syntaxMark,
	"%":         "sleep 0.2 & kill %1 && echo " + syntaxMark,
	"(( ... ))": "(( 1 + 1 == 2 )) && echo " + syntaxMark,
	"[[ ... ]]": "[[ abc =~ ^a.c$ ]] && echo " + syntaxMark,
	"{ ... }":   "{ echo " + syntaxMark + "; }",
	"case":      "case x in x) echo " + syntaxMark + ";; esac",
	"coproc":    `coproc c { echo ` + syntaxMark + `; }; read -r r <&"${c[0]}"; echo "$r"`,
	"for":       "for w in " + syntaxMark + `; do echo "$w"; done`,
	"for ((":    "for ((i=0;i<1;i++)); do echo " + syntaxMark + "; done",
	"function":  "function f { echo " + syntaxMark + "; }; f",
	"if":        "if true; then echo " + syntaxMark + "; else echo wrong; fi",
	"select":    `printf '1\n' | select w in a; do echo "` + syntaxMark + `:$w"; break; done`,
	"time":      "time echo " + syntaxMark,
	"until":     "i=0; until (( i )); do i=1; echo " + syntaxMark + "; done",
	"variables": `[ -n "$PWD" ] && [ -n "$BASH_VERSION" ] && [ -n "$KOI_VERSION" ] && echo ` + syntaxMark,
	"while":     "i=0; while (( ! i )); do i=1; echo " + syntaxMark + "; done",
}

// TestHelpSyntaxTopicsAreConstructsKoiRuns is the drift guard for the
// syntax half of the help table, in both directions: every topic is
// listed by `compgen -A helptopic`, answers `help` with real text, and
// runs — and `suspend`, which bash lists and koi refuses, does neither.
func TestHelpSyntaxTopicsAreConstructsKoiRuns(t *testing.T) {
	t.Parallel()
	koi := buildKoi(t)
	dir := t.TempDir()

	topics, _ := shellLines(t, koi, dir, "compgen -A helptopic")
	if len(topics) == 0 {
		t.Fatal("compgen -A helptopic listed nothing")
	}

	for topic, probe := range helpSyntaxProbes {
		t.Run(topic, func(t *testing.T) {
			t.Parallel()
			if !slices.Contains(topics, topic) {
				t.Errorf("help answers about %q but compgen -A helptopic does not offer it", topic)
			}

			out, status := shellRows(t, koi, dir, "help "+singleQuoted(topic))
			if status != 0 {
				t.Fatalf("help %q exited %d: %q", topic, status, out)
			}
			if len(out) < 2 {
				t.Fatalf("help %q printed %q, want a synopsis and a description", topic, out)
			}
			if !strings.HasPrefix(out[0], topic+": ") || len(out[0]) <= len(topic)+2 {
				t.Errorf("help %q headed its entry %q", topic, out[0])
			}
			if len(strings.TrimSpace(out[1])) < 20 {
				t.Errorf("help %q described it as %q", topic, out[1])
			}

			ran, status := shellRows(t, koi, dir, probe)
			if status != 0 || !slices.ContainsFunc(ran, func(s string) bool {
				return strings.Contains(s, syntaxMark)
			}) {
				t.Errorf("help lists %q but %q gave %q (status %d)", topic, probe, ran, status)
			}
		})
	}

	// The other direction, and the reason this is not just a list: bash
	// has a `suspend` topic and koi must not, because koi refuses the
	// builtin. Both halves are asserted — "the listing lacks it" passes
	// vacuously against an empty listing, which the length check above
	// and the per-topic membership checks rule out — and the refusal is
	// asserted by running it, so the day `suspend` starts working this
	// fails instead of quietly going stale.
	if slices.Contains(topics, "suspend") {
		t.Error("compgen offers a `suspend` help topic for a builtin koi refuses")
	}
	out, status := runHermetic(t, koi, "suspend")
	if status == 0 || !strings.Contains(out, "unsupported builtin") {
		t.Errorf("suspend answered %q (status %d) — it works now, so it needs a help topic", out, status)
	}
}
