//go:build unix

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Where a *native* builtin's diagnostic says it came from (#611).
//
// #571 built the location prefix and handed it to the exec handler as
// HandlerContext.ErrLocation; #584 gave it to the word-expansion family.
// koi's own builtins — the ones in internal/builtins reached at the exec
// seam, and the ones in internal/repl reached by renaming the call
// (#565) — were the third family, and printed `history: -x: invalid
// option` where bash prints `./history.tests: line 17: history: -x:
// invalid option`.
//
// The fix is one pair of helpers on the handler context, so the test has
// to be able to fail when a *new* native builtin forgets to use them.
// That is what the two guards below do, in the same shape the two
// builtins matrices use on each other: a name koi intercepts with no
// case here fails, and a case here for a name nothing intercepts fails.

// nativeDiagCase is one native builtin's diagnostic.
type nativeDiagCase struct {
	// script is a single command line that provokes a diagnostic. It is
	// written into a script *file*, because that is the only place a
	// location exists — koi's $0 is `koi` (#120), so a command string
	// and standard input deliberately carry none.
	script string
	// want is the diagnostic as it reads *after* the location prefix.
	// A prefix of the message rather than the whole of it, so wording
	// this issue is not about stays free to change.
	want string
	// noDiag records that this builtin has no diagnostic a script can
	// provoke, with the reason. Every entry needs one: a name that opted
	// out silently is how a family of unlocated messages survives a test
	// that is supposed to cover it.
	noDiag string
	// bashOracle marks the cases where bash has the same builtin and
	// says the same thing, so the whole line can be compared against it
	// rather than asserted from memory.
	bashOracle bool
}

// nativeDiagCases covers every builtin koi answers natively.
//
// The scripts are deliberately dull — no PATH lookups, no timing, no
// network — because a case that fails on an unusual machine teaches
// nothing about locations.
var nativeDiagCases = map[string]nativeDiagCase{
	// The interpreter-claimed names koi replaces. bash has all of these
	// and locates all of them, so bash is the oracle for the whole line.
	"printf":  {script: `printf "%y" x`, want: "printf: `%y': invalid format character"},
	"kill":    {script: "kill -Q 1", want: "kill: -Q: invalid signal specification"},
	"umask":   {script: "umask -x", want: "umask: -x: invalid option", bashOracle: true},
	"history": {script: "history -x", want: "history: -x: invalid option", bashOracle: true},
	"fc":      {script: "fc -x", want: "fc: -x: invalid option"},
	"help":    {script: "help nosuchtopic_for_this_test", want: "help: no help topic for"},
	"newgrp":  {script: "newgrp", want: "newgrp: not provided by koi"},

	// koi's own commands, which have no oracle: the assertion is that
	// the message is located, not that bash agrees.
	"blocks":   {script: "blocks nosuchsubcommand", want: "blocks: unknown usage"},
	"clip":     {script: "clip -x", want: `clip: unknown argument "-x"`},
	"config":   {script: "config nosuchsetting", want: `config: unknown setting "nosuchsetting"`},
	"explain":  {script: "explain", want: "explain: no AI provider in this session"},
	"migrate":  {script: "migrate --nosuchoption", want: `migrate: unknown option "--nosuchoption"`},
	"parallel": {script: "parallel", want: "parallel: no command"},
	"pick":     {script: "pick -x </dev/null", want: `pick: unknown argument "-x"`},
	"plugin":   {script: "plugin nosuchsubcommand", want: `plugin: unknown arguments "nosuchsubcommand"`},
	"plugins":  {script: "plugins", want: "plugins: the plugin host runs only in an interactive session"},
	"prompt":   {script: "prompt import /nosuchdir_for_this_test/p10k.zsh", want: "prompt: open /nosuchdir_for_this_test"},
	"p10k":     {script: "p10k import /nosuchdir_for_this_test/p10k.zsh", want: "p10k: open /nosuchdir_for_this_test"},
	"sandbox":  {script: "sandbox nosuchcommand", want: "sandbox: missing `--` before the command"},
	"sessions": {script: "sessions restore nosuchsession", want: `sessions: no session matching "nosuchsession"`},
	"tool":     {script: "tool nosuchsubcommand", want: `tool: unknown arguments "nosuchsubcommand"`},
	"trust":    {script: "trust", want: "trust: env plugins are not available in this session"},
	"z":        {script: "z zzz_no_such_directory", want: `z: no match for "zzz_no_such_directory"`},
	"zi":       {script: "zi migrate", want: "zi: migration is unavailable in this session"},

	// The ones with nothing to provoke, each with its reason.
	"builtins": {noDiag: "it takes no arguments and only lists; there is no failing path"},
	"times":    {noDiag: "it validates no options and its only error is a getrusage failure, which a test cannot provoke"},
	"doctor":   {noDiag: "it is advisory by design (#67): every finding is a ✔/⚠/✘ line on stdout, never a diagnostic"},
	"declare_funcs": {
		noDiag: "the `declare -F` rewrite (#215) complains only when there is no session runner, " +
			"and a script always has one",
	},
	"bind": {noDiag: "an unknown option is silently accepted rather than diagnosed — filed as #556"},
	// The three completion builtins share runCompleteBuiltin and share
	// the same gap.
	"complete": {noDiag: "an unknown option is silently accepted rather than diagnosed — filed as #556"},
	"compgen":  {noDiag: "an unknown option is silently accepted rather than diagnosed — filed as #556"},
	"compopt":  {noDiag: "an unknown option is silently accepted rather than diagnosed — filed as #556"},
}

// TestNativeBuiltinDiagnosticsAreLocated is the positive half: in a
// script file, every one of these says which file and which line.
//
// The line is not line 1 on purpose. A prefix built from a constant, or
// from the wrong frame, would still contain the file name — pushing the
// command down the file is what makes the *number* part of the
// assertion.
func TestNativeBuiltinDiagnosticsAreLocated(t *testing.T) {
	if testing.Short() {
		t.Skip("native builtin diagnostic locations skipped in -short")
	}
	koi := buildKoi(t)

	const pad = 6 // blank lines before the command
	for name, tc := range nativeDiagCases {
		if tc.noDiag != "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			body := strings.Repeat("\n", pad) + tc.script + "\n"
			if err := os.WriteFile(filepath.Join(dir, "s.sh"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			out, _ := runInDir(t, dir, koi, "./s.sh")

			prefix := "./s.sh: line " + strconv.Itoa(pad+1) + ": "
			if want := prefix + tc.want; !strings.Contains(out, want) {
				t.Errorf("%s: output does not contain %q\ngot:\n%s", name, want, out)
			}
			// The other half of #571's rule, asserted once for every
			// builtin rather than case by case: the usage line bash
			// prints *after* a diagnostic is not itself a diagnostic and
			// arrives bare. That is why rawErrf exists beside errf, and
			// koi had it backwards for `unalias` (#611).
			for line := range strings.SplitSeq(out, "\n") {
				if strings.HasPrefix(line, "./s.sh: line ") && strings.Contains(line, "usage:") {
					t.Errorf("%s located a usage line, which is not a diagnostic: %q", name, line)
				}
			}
		})
	}
}

// TestNativeBuiltinDiagnosticsCarryNoLocationInACommandString is the
// negative half, and it is what stops the positive one passing
// vacuously. A test that only asserts the prefix is *present* cannot
// tell a working rule from a blanket one — #571's rule is "when there is
// a file to name", and a command string has none.
func TestNativeBuiltinDiagnosticsCarryNoLocationInACommandString(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	koi := buildKoi(t)

	for name, tc := range nativeDiagCases {
		if tc.noDiag != "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			out, _ := runInDir(t, dir, koi, "-c", tc.script)
			// The message still has to arrive — otherwise "no prefix"
			// is satisfied by a command that printed nothing, which is
			// the same vacuous pass from the other side.
			if !strings.Contains(out, tc.want) {
				t.Fatalf("%s: -c output does not contain %q\ngot:\n%s", name, tc.want, out)
			}
			if strings.Contains(out, ": line ") {
				t.Errorf("%s: a command string has no file to name, so it must carry no location; got:\n%s",
					name, out)
			}
		})
	}
}

// TestNativeBuiltinDiagnosticsMatchBash compares the whole located line
// against real bash for the builtins bash has and worded the same way.
//
// It is a small subset by design: koi's wording deliberately differs for
// several of these (#120), and a case that opted out of the oracle to
// hide a difference would be worse than no case at all. What is being
// compared here is the part this issue is about — the prefix — with the
// message identical either side of it.
func TestNativeBuiltinDiagnosticsMatchBash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	const pad = 6
	for name, tc := range nativeDiagCases {
		if !tc.bashOracle {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			body := strings.Repeat("\n", pad) + tc.script + "\n"
			if err := os.WriteFile(filepath.Join(dir, "s.sh"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			bashOut, _ := runInDir(t, dir, bash, "./s.sh")
			koiOut, _ := runInDir(t, dir, koi, "./s.sh")

			// Only the diagnostic line: bash follows several of these
			// with a usage line whose wording is #577's subject, not
			// this one's.
			want := diagLine(bashOut, tc.want)
			if want == "" {
				t.Skipf("bash on this machine does not diagnose %q (%s)", tc.script, bashVersion(t, bash))
			}
			if got := diagLine(koiOut, tc.want); got != want {
				t.Errorf("%s: koi said %q where bash said %q", name, got, want)
			}
		})
	}
}

// TestUsageLinesStandingAloneAreBare is this bug's mirror image, which
// bash's own errors.tests found: koi put a location on a usage line
// where bash prints it bare.
//
// bare `unalias` is the case — bash answers with the usage line and
// nothing else, no prefix, because a usage line is not a diagnostic
// (#571's rawErrf rule) — and koi printed `./s.sh: line 7: unalias:
// usage: …`. It is differential because which lines bash locates is
// exactly the thing being claimed here, and it is separate from
// nativeDiagCases because `unalias` is one of the interpreter's own
// builtins rather than a koi-native one: the rule is the same on both
// sides of the handler seam.
func TestUsageLinesStandingAloneAreBare(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)
	dir := t.TempDir()

	const pad = 6
	body := strings.Repeat("\n", pad) + "unalias\n"
	if err := os.WriteFile(filepath.Join(dir, "u.sh"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	bashOut, _ := runInDir(t, dir, bash, "./u.sh")
	koiOut, _ := runInDir(t, dir, koi, "./u.sh")
	if !strings.Contains(bashOut, "unalias: usage:") {
		t.Skipf("bash on this machine says something else for bare unalias (%s): %q", bashVersion(t, bash), bashOut)
	}
	if strings.Contains(bashOut, "line ") {
		t.Fatalf("the oracle located it after all, so the rule under test is wrong: %q", bashOut)
	}
	if koiOut != bashOut {
		t.Errorf("bare unalias differs from bash\n  bash: %q\n  koi:  %q", bashOut, koiOut)
	}
}

// TestFCUsageIsBashsLineAndTheProseIsInHelp is the other half of #611.
//
// `fc` refusing an option printed bash's one usage line plus eleven more
// of prose — koi's history positions, the shared-history caveat, why the
// editing forms are absent. All true, all in the wrong place: a usage
// line is data a caller may match, and this is the most-read output of
// the command. So the refusal is bash's line byte for byte, and the
// explanation is what `help fc` answers with.
//
// Both halves are asserted, because either alone passes vacuously — a
// one-line usage with the prose deleted rather than moved would satisfy
// the first, and prose in `help` while the usage line kept it too would
// satisfy the second.
func TestFCUsageIsBashsLineAndTheProseIsInHelp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)
	dir := t.TempDir()

	// bash is the oracle for the refusal, so the whole thing is compared
	// rather than pinned to a literal here.
	if err := os.WriteFile(filepath.Join(dir, "fc.sh"), []byte("fc -x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bashOut, _ := runInDir(t, dir, bash, "./fc.sh")
	koiOut, _ := runInDir(t, dir, koi, "./fc.sh")
	if !strings.Contains(bashOut, "fc: usage: fc [-e ename]") {
		t.Skipf("bash on this machine words fc's usage differently (%s): %q", bashVersion(t, bash), bashOut)
	}
	if koiOut != bashOut {
		t.Errorf("fc -x differs from bash\n  bash: %q\n  koi:  %q", bashOut, koiOut)
	}
	// Two lines: the diagnostic and the usage. Eleven more is the bug.
	if got := len(strings.Split(strings.TrimRight(koiOut, "\n"), "\n")); got != 2 {
		t.Errorf("fc -x printed %d lines, want 2 (the diagnostic and one usage line):\n%s", got, koiOut)
	}

	// The explanation has to survive the move, not be deleted by it.
	help, code := runInDir(t, dir, koi, "-c", "help fc")
	if code != 0 {
		t.Fatalf("help fc exited %d: %s", code, help)
	}
	for _, want := range []string{"shared across sessions", "editing forms", "listing half"} {
		if !strings.Contains(help, want) {
			t.Errorf("help fc does not mention %q — the prose was deleted rather than moved:\n%s", want, help)
		}
		if strings.Contains(koiOut, want) {
			t.Errorf("fc -x still prints %q; the explanation belongs in `help fc`, not the usage line", want)
		}
	}
}

// diagLine returns the line of out holding msg, or "".
func diagLine(out, msg string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, msg) {
			return line
		}
	}
	return ""
}

// TestEveryNativeBuiltinHasALocationCase is the drift guard, and the
// reason this file is not a list of the builtins that happened to be
// broken when #611 was filed.
//
// Two directions, the way the builtins matrices check each other. The
// exec-seam half of the list is asked of the *shell* — `builtins` prints
// the native registry — so a new builtin registered there fails this
// test the day it lands, without anyone remembering to edit a slice. The
// CallHandler half is a slice, because those names are rewrites rather
// than registry entries and the shell has no single listing of them; it
// is checked against nativebuiltins_test.go's own list so the two cannot
// drift apart either.
func TestEveryNativeBuiltinHasALocationCase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	t.Parallel()
	koi := buildKoi(t)

	// The registry, from the shell itself.
	names := registryBuiltins(t, koi)
	if len(names) == 0 {
		t.Fatal("`builtins` listed no koi builtins, so this guard cannot detect a missing case")
	}
	// The CallHandler-routed commands and replacements. Every name koi
	// intercepts belongs here the day it is wired into the chain, which
	// is the same rule internal/repl's callHandlerCommands carries.
	names = append(names,
		"bind", "blocks", "clip", "compgen", "complete", "compopt",
		"config", "doctor", "explain", "fc", "help", "history",
		"migrate", "p10k", "pick", "plugin", "printf", "prompt",
		"sandbox", "sessions", "tool", "trust", "z", "zi",
	)

	for _, name := range names {
		if _, ok := nativeDiagCases[name]; !ok {
			t.Errorf("%s is a native builtin with no case in nativeDiagCases: it either says where its "+
				"diagnostics come from, or records why it has none", name)
		}
	}
	for name := range nativeDiagCases {
		if !slices.Contains(names, name) {
			t.Errorf("nativeDiagCases has %s, which nothing intercepts", name)
		}
	}
	// And the interlock with the other matrix: a name added there has to
	// arrive here too. `jobs`, `fg` and `bg` are the deliberate
	// exception — they are registered only for an interactive session
	// with job control, so no script can reach them.
	interactiveOnly := []string{"jobs", "fg", "bg"}
	for _, name := range interceptedNative {
		if slices.Contains(interactiveOnly, name) {
			continue
		}
		if _, ok := nativeDiagCases[name]; !ok {
			t.Errorf("%s has a case in nativeCases but none in nativeDiagCases", name)
		}
	}
}

// registryBuiltins asks the shell for internal/builtins' own registry,
// which is the first group `builtins` prints.
func registryBuiltins(t *testing.T, koi string) []string {
	t.Helper()
	out, code := runInDir(t, t.TempDir(), koi, "-c", "builtins")
	if code != 0 {
		t.Fatalf("`builtins` exited %d: %s", code, out)
	}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "koi builtins:") && i+1 < len(lines) {
			return strings.Fields(lines[i+1])
		}
	}
	t.Fatalf("`builtins` printed no koi-builtins group:\n%s", out)
	return nil
}
