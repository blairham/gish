//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// The builtin matrix (#55): every builtin koi claims to implement, run
// against real bash.
//
// The claim being tested used to live in a list. internal/builtins has
// interpImplemented and interpUnsupported, and its guard exercised four
// of the ~forty names — which is how `alias` sat in a list called
// "implemented" while being a silent no-op. A name in a slice is not
// evidence; running it is.
//
// Differential, reusing the #101 harness so bash on the running machine
// is the oracle. Nothing here encodes what bash "should" do, which
// matters most for the builtins whose behavior is surprising (`shopt`
// defaults, `type` wording, `hash` output). Where koi deliberately
// differs, the case says so and asserts koi's own behavior instead of
// pretending bash agrees.
//
// Scripts are deliberately boring: no PATH lookups beyond the coreutils
// every runner has, no timing, no cwd assumptions. A case that fails
// because the runner is unusual teaches nothing.

// builtinCase is one builtin's exercise.
type builtinCase struct {
	// name is the builtin under test.
	name string
	// script must produce observable output — a builtin that runs and
	// prints nothing cannot be distinguished from one that silently does
	// nothing, which is the exact failure mode being hunted.
	script string
	// koiOnly marks a case where bash is not the oracle, with want
	// holding koi's expected combined output. Every one of these needs
	// a reason: a case that opts out of the oracle to hide a difference
	// is worse than no case.
	koiOnly bool
	want    string
	why     string
	// knownGap records a difference from bash that is real, understood,
	// and not yet fixed. The case still runs and is still compared — it
	// asserts the difference is *still there*, so the day the substrate
	// fixes it the test says so and the entry gets deleted. Deleting the
	// case instead would be how a compat suite quietly stops covering
	// the things it is worst at.
	knownGap string
}

// interpBuiltinCases covers internal/builtins' interpImplemented list.
var interpBuiltinCases = []builtinCase{
	{name: ":", script: `: ; echo "status=$?"`},
	{name: "[", script: `[ 1 = 1 ] && echo yes; [ 1 = 2 ] || echo no`},
	{name: "break", script: `for i in 1 2 3; do [ "$i" = 2 ] && break; echo "$i"; done`},
	{name: "builtin", script: `builtin echo via-builtin`},
	{
		// Both spellings mean "the shell builtin, not a function", and
		// both went straight to the interpreter's table -- past the call
		// seam, which is where koi's *replacements* live (#565). So they
		// reached a printf without %q or --, and an unsupported-builtin
		// refusal for three builtins the plain spelling runs.
		//
		// The `--` case is the one bash's own shopt.tests depends on:
		// `builtin printf -- "--\n"` is its section separator, and koi
		// printed the separator without its newline.
		name: "builtin", script: `builtin printf "%q\n" "a b"
builtin printf -- "--\n"
builtin umask
builtin kill -l 1
builtin times >/dev/null; echo "times=$?"
builtin nosuchthing 2>/dev/null; echo "notbuiltin=$?"
builtin ls 2>/dev/null; echo "program=$?"`,
	},
	{name: "cd", script: `cd /tmp && pwd | sed 's|/private||'`},
	{name: "command", script: `command echo via-command`},
	{
		// The same seam from the other spelling. `command kill` and
		// `command umask` are how a portable script reaches the builtin
		// past a function of the same name; both answered "unsupported
		// builtin" while the bare name worked.
		name: "command", script: `command printf "%q\n" "a b"
command umask
command kill -l 1
command times >/dev/null; echo "times=$?"
command help true | head -1
echo() { echo shadowed; }; command echo not-shadowed
command -v printf`,
	},
	{name: "continue", script: `for i in 1 2 3; do [ "$i" = 2 ] && continue; echo "$i"; done`},
	{
		// `cd "$HOME"` first, and that is load-bearing: the listing
		// abbreviates $HOME to ~, so the gap could only show when the
		// directory on the stack is under $HOME. A bare `dirs` used to
		// exercise it by accident, because the runner's cwd happened to
		// sit inside the developer's home — and when #260 gave the
		// runner a scratch home, the two shells started agreeing and
		// this entry looked like a gap that had closed. It had not; the
		// test had stopped asking. #390 closed it for real.
		// -l prints absolute paths, and each shell gets its own
		// scratch home, so the home entry is filtered out of that one
		// line — the difference there would not be koi's.
		name: "dirs", script: `cd "$HOME"; dirs; pushd /tmp >/dev/null; dirs -v; dirs -l | cut -d" " -f1; dirs +1`,
	},
	{
		// The clustered forms are #399: `-ne` was printed as an
		// operand, which is the whole of strip.tests.
		name: "echo", script: `echo plain; echo -n no-newline; echo
echo -ne "ab\ncd"; echo END
echo -en x; echo E
echo -nx keeps-the-flag-as-a-word
echo`,
	},
	{name: "eval", script: `eval 'echo evaluated'`},
	{name: "exec", script: `(exec echo execed); echo after`},
	{name: "exit", script: `(exit 3); echo "status=$?"`},
	{name: "export", script: `export FOO=bar; sh -c 'echo "$FOO"'`},
	{name: "false", script: `false; echo "status=$?"`},
	{name: "getopts", script: `set -- -a -b val; while getopts "ab:" o; do echo "opt=$o arg=$OPTARG"; done`},
	{name: "hash", script: `hash sed; echo "status=$?"`},
	{name: "local", script: `f() { local v=inner; echo "$v"; }; v=outer; f; echo "$v"`},
	{name: "mapfile", script: `printf 'a\nb\n' | { mapfile -t arr; echo "${arr[0]}-${arr[1]}"; }`},
	{name: "popd", script: `pushd /tmp >/dev/null; popd >/dev/null; echo "status=$?"`},
	{
		// The byte cases pin #377: \NNN and \xHH are bytes on the wire,
		// never code points re-encoded as UTF-8; overflow wraps mod 256;
		// and %q renders a byte that is not valid UTF-8 as an octal
		// escape inside $'...' rather than corrupting it to U+FFFD.
		name: "printf", script: `printf '%s-%d-%05.2f\n' str 42 3.14159
printf '\303\251' | wc -c | tr -d ' '
printf '%b' '\303\251' | wc -c | tr -d ' '
printf '\xc3\xa9' | wc -c | tr -d ' '
[ "$(printf '\401')" = "$(printf '\001')" ] && echo wraps
printf '%q\n' "$(printf 'B\315')" "$(printf 'a\tb')" 'a b'
printf '%(%Y-%m-%d)T\n' 0
printf '%ld %lld %ls\n' 5 6 ab
printf '%Q\n' 'a b'
printf -- '--%b--\n' '\\'"'"'abcd'
printf 'fmt:\\'"'"'b\n'
printf '%10000000000d\n' 1 2>/dev/null; echo "rc=$?"
printf '%c' абв | od -An -c | tr -s ' '
LC_ALL=C printf '%c' абв | od -An -c | tr -s ' '
printf '[%c%c]' "" x | od -An -c | tr -s ' '`,
	},
	{name: "pushd", script: `pushd /tmp >/dev/null && pwd | sed 's|/private||'`},
	{name: "pwd", script: `cd /tmp && pwd | sed 's|/private||'`},
	{
		// The byte cases pin #377: a byte that is not valid UTF-8 passes
		// through read and field splitting untouched, a high byte works
		// as -d's delimiter, and -n/-N count characters rather than
		// bytes — five of абвгдежз is абвгд.
		// The counting is exercised in *both* locales now (#470): koi
		// used to be UTF-8 always, so the -n/-N lines needed a UTF-8
		// pin to avoid comparing that posture against bash's C-locale
		// one. It follows the locale, so the pin is the test rather
		// than a workaround.
		name: "read", script: `printf 'one two\n' | { read a b; echo "$a|$b"; }
printf 'B\315\n' | { IFS= read -r f; printf '%s' "$f" | wc -c | tr -d ' '; }
printf 'ab\200cd' | { read -rd "$(printf '\200')" s; echo "$s"; }
export LC_ALL=en_US.UTF-8
read -n 5 foo <<< "абвгдежз"; echo "$foo"
read -N 3 foo <<< "абвгд"; echo "$foo"
export LC_ALL=C
read -n 5 foo <<< "абвгдежз"; printf '%s' "$foo" | wc -c | tr -d ' '
a=$'\316\261'; echo "${#a}"
case $a in ?) echo one;; ??) echo two;; esac`,
	},
	{name: "readarray", script: `printf 'x\ny\n' | { readarray -t arr; echo "${arr[1]}"; }`},
	{
		// The one gap here with teeth: the value is protected either way,
		// but koi reports success. A script using readonly as a guard
		// under `set -e` keeps going where bash would stop.
		name: "readonly", script: `readonly R=fixed; echo "$R"; R=changed 2>/dev/null; echo "$R"`,
		knownGap: "assigning to a readonly variable is ignored silently instead of failing with status 1",
	},
	{name: "return", script: `f() { return 7; }; f; echo "status=$?"`},
	{name: "set", script: `set -- a b c; echo "$#-$1-$3"`},
	{
		// The editing-mode options, which have to run through the
		// *shell* rather than interp's table for the same reason the
		// cdspell case below does (#575, #576): the layer that used to
		// throw the request away was internal/repl's rewrite of the four
		// `set -o vi` forms to a KOI_EDIT_MODE assignment, so the
		// interpreter never saw the `set` and the option kept saying
		// emacs in a shell that was editing in vi.
		name: "set", script: `set -o vi; set -o | grep -E '^(emacs|vi) '
set -o emacs; set -o | grep -E '^(emacs|vi) '
set -o vi; set +o vi; set -o | grep -E '^(emacs|vi) '
set -o vi; case :$SHELLOPTS: in *:vi:*) echo "shellopts=yes";; *) echo "shellopts=no";; esac
shopt -o vi >/dev/null; echo "q=$?"`,
	},
	{name: "shift", script: `set -- a b c; shift; echo "$1-$#"`},
	// The `-p` spelling rather than the column listing, deliberately:
	// the column width is a bash *version* constant (#574) and the
	// oracle here is whatever bash the machine has, so the listing's
	// shape is asserted in TestShoptListingMatchesBash5, which can skip
	// an oracle that predates it. This case is about the option moving.
	{name: "shopt", script: `shopt -s nullglob; shopt -p nullglob`},
	{
		// A query about an option koi accepts and ignores is still a
		// question, and the answer is the interpreter's to give (#566).
		// The accept-and-ignore list used to swallow the name whatever
		// the form, so these printed nothing and -q always said 0 --
		// which is how a script testing an option it never set could
		// take the wrong branch.
		name: "shopt", script: `shopt -p checkjobs; shopt -p histappend; shopt -p checkwinsize
shopt -q checkjobs; echo "q=$?"
shopt -q -o pipefail; echo "qo=$?"
shopt -u histappend; echo "unset=$?"; shopt -p histappend`,
	},
	{
		// An option whose behavior belongs to the line editor rather
		// than to the interpreter still holds its bit, in both
		// directions (#575). This case has to run through the *shell*
		// and not interp's table, because the layer that used to throw
		// the request away is internal/repl's accept-and-ignore list —
		// the interpreter never saw it.
		name: "shopt", script: `shopt -s cdspell histverify; shopt -u checkwinsize progcomp
shopt -p cdspell; shopt -p histverify; shopt -p checkwinsize; shopt -p progcomp
shopt -q cdspell; echo "q=$?"; shopt -q progcomp; echo "qoff=$?"`,
	},
	{
		// The other half of the same rule, and the reason it is not
		// "accept everything": an option that would change what a
		// *script* observes is refused rather than tracked. bash accepts
		// this one, so bash cannot be the oracle for it.
		name: "shopt", script: `shopt -s xpg_echo; echo "st=$?"`,
		koiOnly: true, want: "shopt: unsupported option \"xpg_echo\"\nst=1\n",
		why: "xpg_echo changes what echo does, so tracking the bit would claim a mode koi is not in (#575)",
	},
	{name: "source", script: `echo 'echo sourced' > "$TMPD/s.sh"; . "$TMPD/s.sh"`},
	{name: "test", script: `test -d /tmp && echo isdir; test -z "" && echo empty`},
	{name: "trap", script: `trap 'echo trapped' EXIT; echo body`},
	// The ignore form and its listing live here rather than in interp's
	// table: signal.Ignore is process-global and inherited by children,
	// so an in-process case contaminates every bash oracle the test
	// binary spawns after it. A subprocess per shell cannot.
	{name: "trap", script: `trap "" USR2; trap; trap 'echo h' HUP; trap -p SIGHUP`},
	{name: "true", script: `true; echo "status=$?"`},
	{name: "type", script: `type cd >/dev/null && echo type-ok`},
	{name: "unset", script: `V=set; unset V; echo "[${V:-gone}]"`},
	{name: "wait", script: `sleep 0.01 & wait; echo waited`},
	// kill was "unsupported builtin" until #55. Because the interpreter
	// claims the name, an unimplemented one shadows the machine's own
	// /bin/kill rather than falling through to it — worse than absent.
	// umask was "unsupported builtin" until #55, and it is the one where
	// silence costs something concrete: `umask 077` before writing a key
	// is how a script keeps the file from being world-readable.
	{
		// Symbolic modes describe what to *allow*, so they are read
		// against the mask in force and inverted (#411).
		name: "umask", script: `umask 077; umask; umask -S; umask -p; umask 022; umask
umask u=rwx,g=rwx,o=rx; umask
umask 022; umask a-w; umask
umask 022; umask =; umask
umask 022; umask -S a=rwx; umask
umask 022; umask g-rwx; umask`,
	},
	{
		// -l with operands *translates* rather than listing, which is
		// how a script turns an exit status above 128 into the signal
		// that caused it (#411).
		name: "kill", script: `kill -l >/dev/null; echo "list=$?"; kill 999999 2>/dev/null; echo "badpid=$?"; kill -BOGUS 1 2>/dev/null; echo "badsig=$?"
kill -l 1; kill -l HUP; kill -l 9 15; kill -l 0
kill -l 99 2>/dev/null; echo "badnum=$?"`,
	},

	// alias and unalias cannot be differential: koi expands aliases in
	// interactive sessions only (#53/#163), and bash -c will not expand
	// even with expand_aliases set, because the whole -c string is parsed
	// before the shopt takes effect. The interactive path is covered by
	// TestAliasFromRCWorksInteractively under a real pty; here the script
	// path's job is to stay quiet and bash-shaped.
	{
		name: "alias", script: `alias a='echo aliased'; a 2>/dev/null; echo "status=$?"`,
		koiOnly: true, want: "status=127\n",
		why: "aliases are interactive-only by design; the script path must not expand",
	},
	{
		name: "unalias", script: `alias a='echo x'; unalias a; echo "status=$?"`,
		koiOnly: true, want: "status=0\n",
		why: "unalias of a defined alias succeeds even where expansion is off",
	},
}

// TestInterpBuiltinsActuallyRun is the regression gate: every builtin in
// the implemented list must do its job.
func TestInterpBuiltinsActuallyRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builtin matrix skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	for _, tc := range interpBuiltinCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			script := "TMPD=" + tmp + "\n" + tc.script
			if tc.koiOnly {
				out, _ := runShell(t, koi, script)
				if out != tc.want {
					t.Errorf("koi %s = %q, want %q (%s)", tc.name, out, tc.want, tc.why)
				}
				return
			}
			r := compat.Run(context.Background(), bash, koi, compat.Case{
				Name: tc.name, Script: script,
			})
			// The oracle has a version. macOS still ships bash 3.2 as
			// /bin/bash, which predates mapfile and readarray by a whole
			// major release, so on that runner bash answers "command not
			// found" for a builtin koi implements correctly. Comparing
			// against that would report koi as broken for being newer.
			//
			// Detected from the oracle's own output rather than by
			// version-gating a list, because the list would be another
			// claim needing its own maintenance — the same mistake this
			// file exists to correct.
			if strings.Contains(r.BashOut, tc.name+": command not found") {
				t.Skipf("bash on this machine has no %s (%s) — no oracle for this case",
					tc.name, bashVersion(t, bash))
			}
			if tc.knownGap != "" {
				if r.Pass {
					t.Errorf("%s now matches bash — the gap closed, so delete this knownGap entry: %s",
						tc.name, tc.knownGap)
				} else {
					t.Logf("known gap: %s\n  bash: %q\n  koi: %q", tc.knownGap, r.BashOut, r.KoiOut)
				}
				return
			}
			if !r.Pass {
				t.Errorf("%s differs from bash (%s)\n  bash: %q (exit %d)\n  koi: %q (exit %d)",
					tc.name, r.Reason, r.BashOut, r.BashCode, r.KoiOut, r.KoiCode)
			}
			// A builtin that prints nothing under both shells proves
			// nothing: agreeing on silence is what a missing builtin
			// looks like.
			if r.BashOut == "" {
				t.Errorf("%s: case produces no output, so it cannot detect a no-op", tc.name)
			}
		})
	}
}

// requireBash skips rather than fails when bash is absent: the oracle is
// the machine's bash, and a machine without one cannot run a
// differential test. Skipping is honest; asserting from memory is not.
// bashVersion reports the oracle's version, for skip messages that say
// which bash could not answer.
func bashVersion(t *testing.T, bash string) string {
	t.Helper()
	out, err := exec.Command(bash, "-c", "echo $BASH_VERSION").Output()
	if err != nil {
		return "unknown version"
	}
	return "bash " + strings.TrimSpace(string(out))
}

func requireBash(t *testing.T) string {
	t.Helper()
	// KOI_TEST_BASH points the oracle at a specific bash. macOS ships
	// 3.2 as /bin/bash while most developers have 5.x first on PATH, so
	// without this the CI-only failures (a builtin the old oracle lacks)
	// cannot be reproduced locally — which is how one shipped.
	if b := os.Getenv("KOI_TEST_BASH"); b != "" {
		return b
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed: nothing to be differential against")
	}
	return bash
}

// runShell runs a script through a shell's -c and returns combined
// output and exit status — the same shape compat.Run compares, so a
// koi-only case and a differential case are measuring the same thing.
func runShell(t *testing.T, bin, script string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, "-c", script)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", bin, err)
	}
	return string(out), code
}
