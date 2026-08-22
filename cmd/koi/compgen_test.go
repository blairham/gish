//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// compgen's candidate classes, differentially (#269).
//
// These are answers about a real filesystem, so both shells are pointed
// at the same fixture tree and their whole output is compared. A unit
// test could only assert what this file believes bash does, which is the
// thing that was wrong: the trailing slash looked right until you asked
// bash.
//
// bash is the oracle. Nothing below encodes an expected listing.

// compgenFixture builds the tree every path case is asked about: two
// plain directories, a hidden one, a nested one, plain and hidden files,
// and symlinks to a directory and a file — the last because -d follows
// links and a broken link is still a name -f must return.
func compgenFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"adir", ".hdir", "sub", "empty"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"afile", ".hidden", "sub/x", "sub/y"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, ln := range [][2]string{{"adir", "linkdir"}, {"afile", "linkfile"}, {"nowhere", "broken"}} {
		if err := os.Symlink(ln[0], filepath.Join(dir, ln[1])); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// shellLines runs one -c script under a shell in dir and returns its
// sorted output and exit status. Sorted because bash returns readdir
// order and koi sorts; the issue calls that difference harmless, and
// what is being compared here is the set of names and their shape.
//
// Sorting is this caller's need and not the runner's — anything checking
// output whose *order* is part of the answer wants shellRows instead.
func shellLines(t *testing.T, shell, dir, script string) ([]string, int) {
	t.Helper()
	lines, status := shellRows(t, shell, dir, script)
	sort.Strings(lines)
	return lines, status
}

// shellRows runs one -c script and returns its output as written.
func shellRows(t *testing.T, shell, dir, script string) ([]string, int) {
	t.Helper()
	cmd := exec.Command(shell, "-c", script)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb"}
	out, err := cmd.Output()
	status := 0
	var exitErr *exec.ExitError
	if err != nil {
		if !asExitError(err, &exitErr) {
			t.Fatal(err)
		}
		status = exitErr.ExitCode()
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	return lines, status
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError) //nolint:errorlint // the only wrapping here is none
	if ok {
		*target = e
	}
	return ok
}

func TestCompgenPathsMatchBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	// Each case is the compgen argument list, run verbatim under both.
	tests := []struct {
		name string
		args string
	}{
		// The two the issue opened on: bare names, no trailing separator.
		{"files in the current directory", "-f"},
		{"directories in the current directory", "-d"},
		{"the directory action spelled out", "-A directory"},

		// A candidate carries the directory part as it was typed, so a
		// completion function asking about a subdirectory gets names
		// under it — not the cwd's, which is what a cwd-only listing
		// answered before the word reached the generator.
		{"a subdirectory prefix", "-f sub/"},
		{"a subdirectory prefix, directories only", "-d ./"},
		{"a dot-slash prefix", "-f ./"},
		{"an absolute prefix", "-d /usr/l"},

		// Dot entries: present whatever the prefix for -f, and . and ..
		// only once the word asks for a dot.
		{"a dot prefix", "-f ."},
		{"a dot prefix, directories only", "-d ."},
		{"the parent alone", "-f .."},
		{"a hidden-file prefix", "-f .h"},
		{"a dot prefix under a subdirectory", "-f sub/."},
		{"a dot prefix in an empty directory", "-f empty/."},

		// Nothing generated is a failure, and an unreadable directory
		// generates nothing — including the . and .. that would exist if
		// it did.
		{"a directory that is not there", "-f nosuchdir/"},
		{"a dot prefix under a directory that is not there", "-f nosuchdir/."},
		{"an empty directory", "-f empty/"},
		{"a prefix nothing matches", "-f zzz"},

		// The form completion functions actually write.
		{"the separator form", "-f -- ."},
		{"a plain prefix", "-f a"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			script := "compgen " + tc.args
			gotOut, gotStatus := shellLines(t, koi, compgenFixture(t), script)
			wantOut, wantStatus := shellLines(t, bash, compgenFixture(t), script)
			if strings.Join(gotOut, "\n") != strings.Join(wantOut, "\n") {
				t.Errorf("%s:\nkoi:  %q\nbash: %q", script, gotOut, wantOut)
			}
			if gotStatus != wantStatus {
				t.Errorf("%s: koi status %d, bash %d", script, gotStatus, wantStatus)
			}
		})
	}
}

// The keyword action is the one class that cannot be compared outright:
// bash 4.0 and later have coproc and koi does not implement it (#287),
// so answering with it would be a lie about this shell. Everything else
// must match, in both directions — a keyword bash lists and koi does not
// is the bug the issue reported, and one koi lists and bash does not
// would mean inventing a keyword.
//
// coproc is allowed to be missing rather than required to be: the oracle
// here is whichever bash is on PATH, and macOS ships 3.2, whose own list
// predates coproc. Asserting the difference *is* coproc passed against
// 5.3 and failed on the macOS runner, where koi matched bash exactly.
// Subset keeps the claim that matters — a short list still fails, which
// is the regression this guards.
func TestCompgenKeywordsMatchBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	got, _ := shellLines(t, koi, dir, "compgen -k")
	want, _ := shellLines(t, bash, dir, "compgen -k")

	for _, kw := range difference(want, got) {
		t.Errorf("bash lists keyword %q and koi does not", kw)
	}
	for _, kw := range difference(got, want) {
		// The oracle has a version, and koi's list is bash 5.3's. macOS
		// ships 3.2 as /bin/bash, which predates `coproc` by a major
		// release — so there, koi listing it is koi being newer rather
		// than koi being wrong. Confirmed against the oracle instead of
		// assumed, so this stops excusing it the moment the runner's
		// bash grows the keyword.
		if kw == featCoproc && !oracleHas(t, bash, featCoproc) {
			t.Logf("bash here has no %s (%s); koi listing it is not a mismatch", kw, bashVersion(t, bash))
			continue
		}
		t.Errorf("koi lists keyword %q and bash does not", kw)
	}

	// The six that were missing all work, which is why the list was a
	// reporting bug and not an honest refusal. coproc joined them in
	// #287: it used to be checked the other way — off the list for as
	// long as it failed — and it now runs, so it is listed and exercised
	// like the rest.
	for _, tc := range []struct{ name, script string }{
		{"[[", "[[ 1 == 1 ]] && echo ok"},
		{"{", "{ echo ok; }"},
		{"!", "! false && echo ok"},
		{"in", "for i in ok; do echo $i; done"},
		{"coproc", `coproc c { echo ok; }; read -r r <&"${c[0]}"; echo "$r"`},
	} {
		out, status := shellLines(t, koi, dir, tc.script)
		if status != 0 || strings.Join(out, "") != "ok" {
			t.Errorf("koi lists %q as a keyword but %q gave %q (status %d)", tc.name, tc.script, out, status)
		}
	}
}

func difference(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

// The `-A` actions that name a *shell table* rather than the file system
// (#277): `setopt`, `shopt`, `enabled`, `disabled` and `helptopic` all
// answered nothing, which is indistinguishable from a shell with no
// options and no builtins — and it is what a completion script asks
// before offering `set -o <TAB>`.
//
// Two of them can match bash exactly, because koi recognizes every name
// bash does even where it keeps one at its default; the other two are
// koi's own set by the same rule that makes `compgen -b` answer 51 to
// bash's 61 (#269), so they are asserted against koi rather than
// compared.
func TestCompgenOptionActionsMatchBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	for _, action := range []string{"setopt", "shopt", "disabled"} {
		got, _ := shellLines(t, koi, dir, "compgen -A "+action)
		want, _ := shellLines(t, bash, dir, "compgen -A "+action)
		for _, name := range difference(want, got) {
			t.Errorf("bash lists %s %q and koi does not", action, name)
		}
		for _, name := range difference(got, want) {
			t.Errorf("koi lists %s %q and bash does not", action, name)
		}
		if action != "disabled" && len(got) == 0 {
			t.Errorf("compgen -A %s listed nothing, which is what this test exists to catch", action)
		}
	}
}

// The two whose answer is this shell's own set, checked for being true
// rather than for being bash's: a name listed here has to be one koi
// can act on, which is the property that makes the listing worth having.
func TestCompgenListsOnlyWhatKoiHas(t *testing.T) {
	t.Parallel()
	koi := buildKoi(t)
	dir := t.TempDir()

	topics, _ := shellLines(t, koi, dir, "compgen -A helptopic")
	if len(topics) == 0 {
		t.Fatal("compgen -A helptopic listed nothing")
	}
	for _, topic := range topics {
		out, status := shellRows(t, koi, dir, "help "+singleQuoted(topic))
		if status != 0 {
			t.Errorf("compgen offers help topic %q, which help itself does not answer", topic)
			continue
		}
		// A zero status is not enough: an entry with an empty synopsis
		// or an empty description prints two blank-ish lines and
		// succeeds, which is a listed topic with nothing behind it
		// (#557). The rewrite names answer with one pointing line, so
		// the assertion is about the first line carrying more than the
		// topic's own name.
		if len(out) == 0 || len(strings.TrimSpace(out[0])) <= len(topic)+1 {
			t.Errorf("compgen offers help topic %q and help answered %q", topic, out)
		}
	}

	enabled, _ := shellLines(t, koi, dir, "compgen -A enabled")
	builtinList, _ := shellLines(t, koi, dir, "compgen -b")
	for _, name := range difference(enabled, builtinList) {
		t.Errorf("compgen -A enabled lists %q, which is not a builtin here", name)
	}
	for _, name := range difference(builtinList, enabled) {
		// koi has no `enable -n`, so every builtin it has is enabled;
		// the two lists are the same one until that changes.
		t.Errorf("compgen -b lists %q but -A enabled does not", name)
	}
}

// singleQuoted wraps a topic for the shell, since some of them are
// punctuation the shell would otherwise read.
func singleQuoted(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// The complete/compgen/compopt option grammar (#556).
//
// Two halves, and both are asserted, because either alone passes
// vacuously. A shell that refused *every* option would pass the refusal
// table; a shell that accepted every option — koi, until this change —
// passes the acceptance table. The pair is the claim.
//
// The refusals compare the exit status against bash and check that the
// message tail appears in *both* shells' output. That second half is what
// keeps the wording honest: koi's message is bash's because bash printed
// it here, not because this file believes it does. What is deliberately
// not compared is the whole line — bash prefixes a location koi's own
// builtins do not carry yet (#621) and follows the message with a usage
// line (#577), so the status and the wording are what is pinned.

// compFamilyRefusals are the invocations bash refuses, one per shape of
// refusal. Every one of them was exit 0 or a silent 1 in koi before, and
// `complete` was the worst: 0 says the registration happened.
var compFamilyRefusals = []struct {
	name, script, message string
}{
	{"an unknown option to complete", `complete -Z x`, "complete: -Z: invalid option"},
	{"an unknown option to compgen", `compgen -Z x`, "compgen: -Z: invalid option"},
	{"an unknown option to compopt", `compopt -Z x`, "compopt: -Z: invalid option"},

	// -V is compgen's alone, which is what bash's own complete.tests
	// records as "doesn't work for complete".
	{"complete does not take -V", `complete -V name`, "complete: -V: invalid option"},
	// The options complete has and compgen does not, and vice versa.
	{"compgen does not take -p", `compgen -p`, "compgen: -p: invalid option"},
	{"compgen does not take -r", `compgen -r`, "compgen: -r: invalid option"},
	{"compgen does not take -D", `compgen -D`, "compgen: -D: invalid option"},
	{"compopt does not take -p", `compopt -p x`, "compopt: -p: invalid option"},

	// An unknown letter inside a cluster, so the cluster is read rather
	// than taken as one long option name.
	{"an unknown letter in a cluster", `complete -dZ x`, "complete: -Z: invalid option"},

	// A `+`-word is an option for compopt only, and there it can be wrong.
	{"an unknown plus option to compopt", `compopt +Z x`, "compopt: +Z: invalid option"},

	// An option that takes an argument and did not get one. koi read the
	// missing argument as the empty string, so `compgen -W` generated
	// from an empty wordlist and said nothing.
	{"a missing option argument", `compgen -W "a b" -V`, "compgen: -V: option requires an argument"},

	// The same bug at the value level: a name outside a closed list was
	// taken verbatim and then quietly did nothing.
	{"an unknown -o name", `compgen -o nooption -W a`, "compgen: nooption: invalid option name"},
	{"an unknown -o name to complete", `complete -o nooption x`, "complete: nooption: invalid option name"},
	{"an unknown -o name to compopt", `compopt -o nooption x`, "compopt: nooption: invalid option name"},
	{"an unknown -A action", `compgen -A noaction`, "compgen: noaction: invalid action name"},
	{"an unknown -A action to complete", `complete -A noaction x`, "complete: noaction: invalid action name"},

	// -V takes a name, not an assignment target: bash refuses a subscript
	// here where `printf -v` accepts one.
	{"a -V name that is not an identifier", `compgen -V invalid-name -b`, "compgen: `invalid-name': not a valid identifier"},
	{"a -V name with a subscript", `compgen -V 'arr[2]' -b`, "compgen: `arr[2]': not a valid identifier"},
}

// compFamilyAccepted are the invocations bash *accepts*, which is the
// half that makes the refusal safe to have. Each one is a shape the
// refusal would reject if the argument list were read a word at a time,
// and one of them (`complete -df x`) is ordinary bash-completion corpus.
var compFamilyAccepted = []struct {
	name, script string
}{
	{"clustered letters", `complete -df x`},
	{"a cluster ending in an argument-taking letter", `compgen -bW "aa bb"`},
	{"an argument joined to its letter", `compgen -bWfoo`},

	// bash does not permute: options stop at the first operand, so a
	// dash-word after one is a name rather than an option.
	{"an option-looking name", `complete x -Z`},
	{"an option-looking word", `compgen -W abc abc -Z`},

	// And a `+`-word is not an option for these two at all — it is a
	// name. bash registers three specs here, not one with nosort cleared.
	{"a plus word is a name", `complete +o nosort x`},
	{"a plus word is a word", `compgen +b`},

	// Every -o name and every -A action bash knows, so the closed lists
	// cannot go stale in the refusing direction.
	// fullquote is bash 5.3's ninth and koi refused it until #612, which
	// is why this loop is the acceptance half rather than a formality.
	{"every -o name", `for o in bashdefault default dirnames filenames fullquote noquote nosort nospace plusdirs; do complete -o "$o" x || echo "refused -o $o"; done`},
	{"every -A action", `for a in alias arrayvar binding builtin command directory disabled enabled export file function group helptopic hostname job keyword running service setopt shopt signal stopped user variable; do compgen -A "$a" >/dev/null; [ $? = 2 ] && echo "refused -A $a"; done`},

	// The three catch-alls, -I included: bash 5.1 added it and refusing
	// it would reject a line bash takes. It is consulted for the command
	// position as of #609; this case only asserts the registration is
	// accepted.
	{"the catch-all registrations", `complete -D -F f; complete -E -F f; complete -I -F f`},
}

func TestCompFamilyRefusesWhatBashRefuses(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	for _, tc := range compFamilyRefusals {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			script := tc.script + `; echo "rc=$?"`
			gotOut, _ := runShell(t, koi, script)
			wantOut, _ := runShell(t, bash, script)

			// The status the caller acts on, taken from the shell rather
			// than from the process: `echo "rc=$?"` is what a completion
			// script would read.
			wantRC := grepLine(wantOut, "rc=")
			if wantRC != "rc=2" {
				t.Skipf("bash here does not refuse %q (%s): %q",
					tc.script, bashVersion(t, bash), wantOut)
			}
			if got := grepLine(gotOut, "rc="); got != wantRC {
				t.Errorf("%s: koi %s, bash %s\n  koi:  %q\n  bash: %q",
					tc.script, got, wantRC, gotOut, wantOut)
			}
			if !strings.Contains(wantOut, tc.message) {
				t.Errorf("%s: bash does not say %q — the expected message is invented: %q",
					tc.script, tc.message, wantOut)
			}
			if !strings.Contains(gotOut, tc.message) {
				t.Errorf("%s: koi does not say %q: %q", tc.script, tc.message, gotOut)
			}
		})
	}

	for _, tc := range compFamilyAccepted {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			script := tc.script + `; echo "rc=$?"`
			gotOut, _ := runShell(t, koi, script)
			wantOut, _ := runShell(t, bash, script)
			if strings.Contains(wantOut, "invalid option") || strings.Contains(wantOut, "refused ") {
				t.Skipf("bash here refuses %q (%s): %q", tc.script, bashVersion(t, bash), wantOut)
			}
			if strings.Contains(gotOut, "invalid option") || strings.Contains(gotOut, "refused ") {
				t.Errorf("%s: koi refuses what bash accepts: %q", tc.script, gotOut)
			}
			if got, want := grepLine(gotOut, "rc="), grepLine(wantOut, "rc="); got != want {
				t.Errorf("%s: koi %s, bash %s\n  koi:  %q\n  bash: %q",
					tc.script, got, want, gotOut, wantOut)
			}
		})
	}
}

// grepLine returns the first line of out starting with prefix, so a case
// can read one reported value out of a script's output without depending
// on what a diagnostic printed around it.
func grepLine(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// `compgen -V name` (#556): bash 5.3's option for putting the candidates
// in an array instead of on stdout — the shape that replaces `arr=(
// $(compgen …) )`, and with it the word splitting that turns a candidate
// containing a space into two.
//
// Differential, and every case is chosen so the *order* of the array is
// not what is being compared: koi sorts compgen's output where bash keeps
// generation order (#613), so a wordlist that is already in order is used
// where the elements are asserted, and the one case that does care about
// order compares each shell against itself.
func TestCompgenVWritesAnArray(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	if !oracleHas(t, bash, featCompgenV) {
		t.Skipf("bash here has no compgen -V (%s) — no oracle for this case", bashVersion(t, bash))
	}

	for _, tc := range []struct{ name, script string }{
		// The array is written and stdout stays empty: `-V` is instead
		// of printing, not as well as. The output goes to a file rather
		// than into `$(…)`, because a command substitution runs compgen
		// in a subshell and the array would then be set where nothing
		// can read it — which is the whole reason `-V` exists.
		{"the candidates land in the array", `f=$(mktemp); compgen -V arr -W "aa bb cc" >"$f"; echo "printed=[$(cat "$f")] rc=$?"; rm -f "$f"; declare -p arr`},

		// It replaces rather than appends, which is the difference
		// between a completion function that offers this word's
		// candidates and one that offers every word's so far.
		{"an existing array is replaced", `arr=(x y z); compgen -V arr -W "aa bb" >/dev/null; declare -p arr`},

		// Nothing generated still creates the array *and* still answers
		// 1, so a caller can read either one.
		{"nothing generated still creates it", `compgen -V arr -W "" -- zzz; echo "rc=$?"; declare -p arr`},
		{"no generator at all", `compgen -V arr; echo "rc=$?"; declare -p arr`},

		// The shaping options apply inside the array, and `-o nosort`
		// changes nothing — compgen does not sort in bash either.
		{"prefix and suffix apply", `compgen -V arr -P "pre-" -S "-suf" -W "aa bb" >/dev/null; declare -p arr`},
		{"nosort changes nothing", `compgen -V arr -o nosort -W "aa bb" >/dev/null; declare -p arr`},

		// The scope is the caller's, which is why the assignment goes
		// back through the interpreter rather than being written by the
		// handler: bash's -V inside a function with a `local` of that
		// name writes the local and leaves the global unset.
		{"a local of that name is what gets written", `f(){ local arr=(z); compgen -V arr -W "aa" >/dev/null; declare -p arr; }; f`},

		// bash's own complete.tests line, which is where the option came
		// to attention: build the `unalias` commands that undo the
		// aliases just defined, minus the ones a pattern filters out.
		{"the suite's own case", `alias fee=one fi=two fo=three fum=four
compgen -a -X 'fo*' -V vv -P 'unalias -- ' f
printf '%s\n' "${vv[@]}"`},

		// A readonly target is refused by the assignment, not by the
		// option parsing, so the status is 1 rather than 2. Only the
		// status is compared here: bash's message carries a location and
		// koi's does not (#621), and koi additionally abandons the rest
		// of the line where bash carries on — a property of a failed
		// assignment (#469) rather than of compgen, recorded rather than
		// worked around.
		{"a readonly target", `readonly ro=1; compgen -V ro -W "aa"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotOut, gotCode := runShell(t, koi, tc.script)
			wantOut, wantCode := runShell(t, bash, tc.script)
			if strings.Contains(tc.script, "readonly") {
				if !strings.Contains(gotOut, "ro: readonly variable") {
					t.Errorf("koi does not report the readonly target: %q", gotOut)
				}
				if !strings.Contains(wantOut, "ro: readonly variable") {
					t.Errorf("bash does not report the readonly target: %q", wantOut)
				}
			} else if gotOut != wantOut {
				t.Errorf("%s:\n  koi:  %q\n  bash: %q", tc.script, gotOut, wantOut)
			}
			if gotCode != wantCode {
				t.Errorf("%s: koi status %d, bash %d", tc.script, gotCode, wantCode)
			}
		})
	}
}

// The array is the listing, in the same order — asserted with each shell
// compared against itself, so it holds without either shell's candidate
// order being encoded here. This is the invariant that would survive
// #613 changing koi's order, and it is what fails if `-V` is ever
// implemented as a second generation pass rather than as the same one.
func TestCompgenVArrayIsTheListing(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	if !oracleHas(t, bash, featCompgenV) {
		t.Skipf("bash here has no compgen -V (%s) — no oracle for this case", bashVersion(t, bash))
	}

	for _, gen := range []string{`-W "cc aa bb"`, `-f`, `-b`, `-A keyword`} {
		script := `[ "$(compgen ` + gen + `)" = "$(compgen -V arr ` + gen +
			` >/dev/null; printf '%s\n' "${arr[@]}")" ] && echo same || echo differs`
		t.Run(gen, func(t *testing.T) {
			t.Parallel()
			dir := compgenFixture(t)
			got, _ := shellRows(t, koi, dir, script)
			want, _ := shellRows(t, bash, dir, script)
			if strings.Join(want, "") != "same" {
				t.Fatalf("bash: %q — the invariant is wrong, not koi", want)
			}
			if strings.Join(got, "") != "same" {
				t.Errorf("koi: compgen %s and its -V array disagree", gen)
			}
		})
	}
}

// Nothing asked for is not a failure. `compgen` with no options at all
// answers 0 in bash — there was no generator to come up empty — where a
// generator that produced nothing answers 1. koi answered 1 for both,
// which is a completion script being told "no candidates" for a question
// it never asked.
func TestCompgenWithNoGeneratorSucceeds(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	for _, script := range []string{
		`compgen; echo "rc=$?"`,
		`compgen abc; echo "rc=$?"`,
		`compgen -- ; echo "rc=$?"`,
		// The other half: a generator that matched nothing still fails,
		// so this is not "compgen always succeeds".
		`compgen -b zzzz; echo "rc=$?"`,
		`compgen -W "aa" zzzz; echo "rc=$?"`,
	} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			gotOut, _ := runShell(t, koi, script)
			wantOut, _ := runShell(t, bash, script)
			if gotOut != wantOut {
				t.Errorf("%s: koi %q, bash %q", script, gotOut, wantOut)
			}
		})
	}
}

// The ten `-A` actions that were recognized and answered nothing (#606).
//
// #277 fixed five actions that name a *shell table*; these are the rest,
// and they split three ways, which is why they are three tests rather
// than one loop. Four of them read the system — users, groups, services
// and hosts — and are compared against bash, because bash's answer there
// is not bash's opinion: `compgen -u` is getpwent(3) and `compgen -A
// hostname` is $HOSTFILE, so the same database is the honest answer and a
// refusal saying koi does not look would be a worse one. `signal`, `job`,
// `running`, `stopped`, `arrayvar` and `binding` are koi's own tables and
// are asserted against koi by #269's rule.
//
// Every one of these asserts *non-empty* as well as whatever else it
// claims, because an empty listing is the bug: "koi lists nothing bash
// does not" passes vacuously against a shell that answers nothing, which
// is exactly the state this issue was opened about.

// compgenSystemActions are the four that read a database outside the
// shell. The comparison is a subset claim rather than equality, and the
// reason is measured: getpwent consults the directory service on a mac
// or an LDAP-joined box, so bash answered 265 users where /etc/passwd
// holds 132 — koi reads the files, since enumerating through libc means
// cgo and Go's os/user has no enumeration to borrow. A name koi lists
// and bash does not is still a failure, and that is the direction a
// misparse breaks in (a leading-whitespace `1023/tcp # Reserved` line in
// darwin's /etc/services answers twenty service names getservent never
// reports).
func TestCompgenSystemActionsMatchBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	// The letter spellings are here for a reason of their own: bash's
	// `-s` is **service**, and `signal` has no letter at all. koi mapped
	// `-s` to `signal`, so the letter form of one action generated the
	// other's candidates — invisible while both answered nothing, and a
	// wrong answer the moment either started working.
	for _, tc := range []struct{ action, opt string }{
		{"user", "-A user"},
		{"user", "-u"},
		{"group", "-A group"},
		{"group", "-g"},
		{"service", "-A service"},
		{"service", "-s"},
		{"hostname", "-A hostname"},
	} {
		action := tc.action
		t.Run(action+" "+tc.opt, func(t *testing.T) {
			t.Parallel()
			script := "compgen " + tc.opt
			got, gotStatus := shellLines(t, koi, dir, script)
			want, wantStatus := shellLines(t, bash, dir, script)
			if len(want) == 0 {
				t.Skipf("bash lists no %s here (%s): no oracle", action, bashVersion(t, bash))
			}
			if len(got) == 0 {
				t.Fatalf("%s listed nothing where bash listed %d — the silence this fixes", script, len(want))
			}
			// Duplicates and order are #613's, and koi still collapses
			// both, so the sets are what is compared here.
			for _, name := range difference(uniqueLines(got), uniqueLines(want)) {
				t.Errorf("koi lists %s %q and bash does not", action, name)
			}
			if gotStatus != wantStatus {
				t.Errorf("%s: koi status %d, bash %d", script, gotStatus, wantStatus)
			}
		})
	}
}

// The host database is the one of the four whose file koi can *own*, so
// it is compared for equality rather than containment — and the parse it
// pins has a rule nobody would guess. readline skips a line's first
// field only when it starts with a digit, so `1.2.3.4 host` lists `host`
// alone while `::1 gamma` lists **both**: an IPv6 address is a hostname
// candidate in bash. A `#` at the start of a field ends the line and
// `a#b` is one name.
func TestCompgenHostnameReadsHostfileLikeBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	hosts := filepath.Join(dir, "hosts")
	if err := os.WriteFile(hosts, []byte(
		"1.2.3.4 alpha beta\n"+
			"# a comment line\n"+
			"\n"+
			"::1 gamma\n"+
			"fe80::1%lo0 delta\n"+
			"5.6.7.8\tepsilon\t# trailing comment\n"+
			"abc.def zeta\n"+
			"10.0.0.1 eta#theta\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, script string }{
		{"the file bash reads", `HOSTFILE=` + hosts + `; compgen -A hostname`},
		{"a prefix", `HOSTFILE=` + hosts + `; compgen -A hostname al`},
		{"a prefix nothing matches", `HOSTFILE=` + hosts + `; compgen -A hostname zzz; echo "rc=$?"`},
		// A file that is not there generates nothing and answers 1, with
		// no diagnostic: the caller is a completion function.
		{"a file that is not there", `HOSTFILE=` + filepath.Join(dir, "nope") + `; compgen -A hostname; echo "rc=$?"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, gotStatus := shellLines(t, koi, dir, tc.script)
			want, wantStatus := shellLines(t, bash, dir, tc.script)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s:\n  koi:  %q\n  bash: %q", tc.script, got, want)
			}
			if gotStatus != wantStatus {
				t.Errorf("%s: koi status %d, bash %d", tc.script, gotStatus, wantStatus)
			}
		})
	}
}

// `-A signal` is koi's own trap table, and the assertion runs in both
// directions because either alone passes vacuously. Nothing listed may
// be a name bash does not have (that would be an invented signal), and
// nothing in koi's own `trap -l` may be missing (that is the silence).
// Every name is then handed to `trap`, which is the property that makes
// the listing worth having: a completion offering a signal the shell
// would refuse is the failure the empty answer was hiding.
func TestCompgenSignalActionIsKoisTrapTable(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	got, _ := shellLines(t, koi, dir, "compgen -A signal")
	want, _ := shellLines(t, bash, dir, "compgen -A signal")
	if len(got) == 0 {
		t.Fatal("compgen -A signal listed nothing, which is what this test exists to catch")
	}
	for _, name := range difference(got, want) {
		t.Errorf("koi lists signal %q and bash does not", name)
	}

	// The fake traps are part of bash's listing and part of koi's, so
	// they are named rather than left to the subset check — they are the
	// four every `trap` in a real script uses.
	for _, pseudo := range []string{"EXIT", "DEBUG", "ERR", "RETURN"} {
		if !slices.Contains(got, pseudo) {
			t.Errorf("compgen -A signal does not offer %q, which koi's trap accepts", pseudo)
		}
	}

	// koi's own listing is the other direction: every signal `trap -l`
	// prints has to be offered, SIG-prefixed as bash spells it.
	listed, _ := shellRows(t, koi, dir, "trap -l")
	tableNames := 0
	for _, row := range listed {
		for _, field := range strings.Fields(row) {
			if !strings.HasPrefix(field, "SIG") {
				continue
			}
			tableNames++
			if !slices.Contains(got, field) {
				t.Errorf("trap -l prints %q and compgen -A signal does not offer it", field)
			}
		}
	}
	if tableNames == 0 {
		t.Error("trap -l printed no signal names, so the direction above proved nothing")
	}

	for _, name := range got {
		out, status := runShell(t, koi, "trap : "+name)
		if status != 0 {
			t.Errorf("compgen offers signal %q, which trap refuses: %q", name, out)
		}
	}
}

// `-A job` and its two filtered forms, against bash. The candidate is
// the job's **first word**, which is measured rather than assumed: bash
// answers `sleep` for `sleep 5 | cat` and `{` for `{ sleep 5; }`, where
// the plausible guess is the whole command line.
//
// Every background command redirects both descriptors, which is the
// harness's need rather than the shell's: a child holding the test's
// stdout keeps the pipe open until it exits, so an unredirected `sleep 5
// &` would cost this test five seconds per case in both shells.
func TestCompgenJobActionsMatchBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()
	bg := ` >/dev/null 2>&1 &`

	for _, tc := range []struct{ name, script string }{
		{"one job", `sleep 5` + bg + ` compgen -A job`},
		// Distinct first words, because two `sleep` jobs are two
		// identical candidates and koi still collapses those (#613).
		{"two jobs", `/bin/sleep 5` + bg + ` sleep 6` + bg + ` compgen -A job`},
		{"a compound job", `{ sleep 5; }` + bg + ` compgen -A job`},
		// Both stages redirect, for the reason above: the *first* stage's
		// stderr is the test's too, and one unredirected descriptor is
		// five seconds.
		{"a pipeline job", `sleep 5 2>/dev/null | cat` + bg + ` compgen -A job`},
		{"the letter form", `sleep 5` + bg + ` compgen -j`},
		{"running only", `sleep 5` + bg + ` compgen -A running`},
		{"a prefix that matches", `sleep 5` + bg + ` compgen -A job sle`},
		{"a prefix that does not", `sleep 5` + bg + ` compgen -A job zzz; echo "rc=$?"`},
		// No jobs is a generator that came up empty: 1, not the 0 of a
		// question never asked.
		{"no jobs at all", `compgen -A job; echo "rc=$?"`},
		{"no stopped jobs", `sleep 5` + bg + ` compgen -A stopped; echo "rc=$?"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, gotStatus := shellLines(t, koi, dir, tc.script)
			want, wantStatus := shellLines(t, bash, dir, tc.script)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s:\n  koi:  %q\n  bash: %q", tc.script, got, want)
			}
			if gotStatus != wantStatus {
				t.Errorf("%s: koi status %d, bash %d", tc.script, gotStatus, wantStatus)
			}
		})
	}
}

// `-A stopped` answers nothing in a koi script, and that is a fact about
// koi rather than a gap: a script's jobs are goroutines, so `kill -STOP
// %1` is *refused* rather than ignored (#397) and there genuinely is
// never a stopped job to list. Asserted as a pair — the empty listing
// beside the refusal that makes it true — because the empty half alone
// is indistinguishable from the bug.
func TestCompgenStoppedIsEmptyBecauseNothingStops(t *testing.T) {
	t.Parallel()
	koi := buildKoi(t)

	out, status := runShell(t, koi, `set -m; sleep 5 >/dev/null 2>&1 & kill -STOP %1; echo "kill=$?"; compgen -A stopped; echo "stopped=$?"`)
	if !strings.Contains(out, "cannot stop a job") {
		t.Errorf("koi did not refuse to stop a job, so an empty -A stopped is a gap: %q", out)
	}
	if !strings.Contains(out, "kill=1") {
		t.Errorf("koi's refusal did not answer 1: %q", out)
	}
	if !strings.Contains(out, "stopped=1") {
		t.Errorf("compgen -A stopped did not answer 1 with nothing generated: %q", out)
	}
	if status != 0 {
		t.Errorf("the script itself failed (status %d): %q", status, out)
	}
}

// The two remaining actions whose answer is this shell's own set, each
// checked for being *true* rather than for being bash's.
func TestCompgenKoiOwnActionsAreTrue(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	// `binding` is readline's function names, and koi's line editor has
	// the ones #96 and #118 gave it — the list comes from the keymap
	// itself, so a name here is a chord the editor really honors. It may
	// never contain a name readline does not have, which is the
	// direction an invented name breaks in.
	bindings, _ := shellLines(t, koi, dir, "compgen -A binding")
	readline, _ := shellLines(t, bash, dir, "compgen -A binding")
	if len(bindings) == 0 {
		t.Fatal("compgen -A binding listed nothing, which is what this test exists to catch")
	}
	if len(readline) == 0 {
		t.Skipf("bash lists no bindings here (%s): no oracle for the subset claim", bashVersion(t, bash))
	}
	for _, name := range difference(bindings, readline) {
		t.Errorf("koi lists binding %q, which readline does not have", name)
	}
	// A spot list rather than the whole keymap: these are the chords the
	// editor's own tests drive, so a mis-wired name shows up here rather
	// than only in a review.
	for _, name := range []string{"accept-line", "backward-char", "kill-line", "yank", "undo", "transpose-chars"} {
		if !slices.Contains(bindings, name) {
			t.Errorf("compgen -A binding does not offer %q, which koi's editor does", name)
		}
	}

	// `arrayvar` is the array-valued half of `-A variable`, so every name
	// it offers has to be one koi will show as an array, and the arrays a
	// script just made have to be in it. Both halves, because "no plain
	// variable is listed" passes vacuously against an empty listing.
	arrays, _ := shellLines(t, koi, dir, `a=(1 2); declare -A m=([k]=v); s=plain; compgen -A arrayvar`)
	if len(arrays) == 0 {
		t.Fatal("compgen -A arrayvar listed nothing")
	}
	for _, want := range []string{"a", "m"} {
		if !slices.Contains(arrays, want) {
			t.Errorf("compgen -A arrayvar does not list %q, which the script just created", want)
		}
	}
	if slices.Contains(arrays, "s") {
		t.Error("compgen -A arrayvar lists a plain string variable")
	}
	for _, name := range arrays {
		out, status := runShell(t, koi, `a=(1 2); declare -A m=([k]=v); s=plain; declare -p `+name)
		if status != 0 || (!strings.Contains(out, "declare -a") && !strings.Contains(out, "declare -A")) {
			t.Errorf("compgen -A arrayvar offers %q, which declare -p does not call an array: %q", name, out)
		}
	}
}

// uniqueLines collapses a sorted listing, so a set comparison is not
// tripped by bash keeping duplicates where koi does not (#613).
func uniqueLines(lines []string) []string {
	return slices.Compact(slices.Clone(lines))
}

// `complete -p` and the three catch-alls (#609).
//
// `complete -D`, `-E` and `-I` live outside `byCommand`, which is all
// `printCompletions` walked — so they were registered and then invisible:
// `eval "$(complete -p)"`, the documented save-and-restore, silently
// dropped every one of them, and `complete -p -D` complained about a name
// containing a NUL byte.
//
// Differential on stdout, because the diagnostics differ by bash's
// `file: line N:` prefix (#621) and its usage second line (#577). Order
// is deliberately *not* what these compare against bash: bash's whole
// listing is a hash-table walk and koi's is sorted (#269), so every case
// that lists more than one spec reads a single name back.
var completeCatchAllCases = []struct{ name, script string }{
	{"printing the default spec", `f(){ :; }; complete -D -o nospace -F f; complete -p -D; echo "rc=$?"`},
	{"printing the empty-line spec", `f(){ :; }; complete -E -F f; complete -p -E; echo "rc=$?"`},
	{"printing the initial-word spec", `f(){ :; }; complete -I -F f; complete -p -I; echo "rc=$?"`},

	// The listing has to *re-register* what it prints, which is the
	// whole point of `complete -p`: a spec printed and then read back
	// must be the same spec.
	{"the listing round-trips", `f(){ :; }; complete -I -o nospace -W 'a b' -F f; saved=$(complete -p -I); complete -r -I; eval "$saved"; complete -p -I; echo "rc=$?"`},

	// Absent is a diagnostic and exit 1, not an empty listing.
	{"the default spec when there is none", `complete -p -D; echo "rc=$?"`},
	{"the empty-line spec when there is none", `complete -p -E; echo "rc=$?"`},
	{"the initial-word spec when there is none", `complete -p -I; echo "rc=$?"`},

	// `complete -r -D` deleted a map entry under the marker and left the
	// default spec exactly where it was.
	{"removing the default spec", `f(){ :; }; complete -D -F f; complete -r -D; complete -p -D; echo "rc=$?"`},
	{"removing everything takes the catch-alls too", `f(){ :; }; complete -D -F f; complete -I -F f; complete -F f foo; complete -r; complete -p; echo "rc=$?"`},

	// A catch-all overrides the operands beside it — the same rule
	// compopt follows (#612) — in all three forms.
	{"a catch-all registration ignores the names", `f(){ :; }; complete -D -F f foo; complete -p -D; complete -p foo; echo "rc=$?"`},
	{"a catch-all removal ignores the names", `f(){ :; }; complete -D -F f; complete -F f foo; complete -r -D foo; complete -p foo; echo "rc=$?"`},
	{"they are mutually exclusive, D over E", `f(){ :; }; complete -D -E -F f; complete -p -D; echo "rc=$?"`},
	{"they are mutually exclusive, E over I", `f(){ :; }; complete -I -E -F f; complete -p -E; echo "rc=$?"`},

	// And a dash-word *after* an operand is an operand, so `-D` there is
	// a name with no spec rather than the default (#556's rule).
	{"a catch-all after an operand is a name", `f(){ :; }; complete -D -F f; complete -F f foo; complete -p foo -D; echo "rc=$?"`},
}

func TestCompleteCatchAllSpecsArePrintable(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	for _, tc := range completeCatchAllCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want, _ := shellRows(t, bash, dir, tc.script)
			got, _ := shellRows(t, koi, dir, tc.script)
			if !slices.Equal(got, want) {
				t.Errorf("%s\n  koi:  %q\n  bash: %q", tc.script, got, want)
			}
		})
	}
}

// The bare listing is the other half, and it cannot be compared line for
// line: bash walks a hash table and koi sorts (#269). What is asserted is
// that all three catch-alls are *in* it — the bug — and that each line
// re-registers its spec.
func TestCompleteListsTheCatchAllSpecs(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()
	script := `f(){ :; }; complete -D -F f; complete -E -F f; complete -I -F f; complete -F f foo; complete -p`

	want, _ := shellLines(t, bash, dir, script)
	got, _ := shellLines(t, koi, dir, script)
	if !slices.Equal(got, want) {
		t.Errorf("%s\n  koi:  %q\n  bash: %q", script, got, want)
	}
	for _, opt := range []string{"-D", "-E", "-I"} {
		if !slices.Contains(got, "complete -F f "+opt) {
			t.Errorf("koi's listing has no %s spec: %q", opt, got)
		}
	}
}

// `compopt` adjusts a completion's options (#612).
//
// koi's compopt parsed its arguments and then answered 0 whatever it was
// asked: it edited no spec, and outside a completion function it claimed
// an adjustment that could not have happened. That is why every case here
// reads back **what changed** rather than the exit status — a builtin
// that already answers 0 by accident passes a status-only assertion, and
// these all did.
//
// Differential on stdout, because the diagnostics differ by bash's
// `file: line N:` prefix (#621) and its usage second line (#577); the
// wording is pinned separately, in both shells, by compoptDiagnostics.
var compoptCases = []struct{ name, script string }{
	// The named form, read back through `complete -p`: the listing the
	// adjustment used to never reach.
	{"adding an option", `f(){ :; }; complete -F f foo; compopt -o nospace foo; echo "rc=$?"; complete -p foo`},
	{"removing an option", `f(){ :; }; complete -o nospace -F f foo; compopt +o nospace foo; echo "rc=$?"; complete -p foo`},
	{"adding one and removing another", `f(){ :; }; complete -o nospace -F f foo; compopt +o nospace -o filenames foo; echo "rc=$?"; complete -p foo`},
	{"adding an option that is already set", `f(){ :; }; complete -o nospace -F f foo; compopt -o nospace foo; echo "rc=$?"; complete -p foo`},
	{"removing an option that is not set", `f(){ :; }; complete -F f foo; compopt +o nospace foo; echo "rc=$?"; complete -p foo`},
	{"a name after --", `f(){ :; }; complete -F f foo; compopt -o nospace -- foo; echo "rc=$?"; complete -p foo`},

	// A name with no spec is reported and the *other* names are still
	// edited: bash carries on rather than abandoning the call.
	{"one name of two has no spec", `f(){ :; }; complete -F f foo; compopt -o nospace nope foo; echo "rc=$?"; complete -p foo`},

	// With no -o at all it is a listing rather than a request, and it is
	// the whole vocabulary with a sign per name — which is also what makes
	// the -o name list itself part of the answer.
	{"the listing form", `f(){ :; }; complete -o nospace -o filenames -F f foo; compopt foo; echo "rc=$?"`},
	{"the listing form for two names", `f(){ :; }; complete -o nospace -F f foo; complete -F f baz; compopt foo baz; echo "rc=$?"`},
	{"the listing form with no options set", `f(){ :; }; complete -F f foo; compopt foo; echo "rc=$?"`},

	// The three catch-alls, read back through compopt's own listing.
	// These were written when `complete -p` could not see a spec outside
	// byCommand at all; #609 fixed that, and they stay on the listing
	// because it is the form that shows every option's state at once.
	{"the default spec", `f(){ :; }; complete -D -F f; compopt -o nospace -D; echo "rc=$?"; compopt -D`},
	{"the empty-line spec", `f(){ :; }; complete -E -F f; compopt -o nospace -E; echo "rc=$?"; compopt -E`},
	{"the initial-word spec", `f(){ :; }; complete -I -F f; compopt -o nospace -I; echo "rc=$?"; compopt -I`},

	// They are mutually exclusive with a fixed priority — D over E over I
	// — whatever order they were written in, which only measuring says.
	{"-E before -D still edits -D", `f(){ :; }; complete -D -F f; complete -E -F f; compopt -o nospace -E -D; echo "rc=$?"; compopt -D; compopt -E`},
	{"-D before -E still edits -D", `f(){ :; }; complete -D -F f; complete -E -F f; compopt -o nospace -D -E; echo "rc=$?"; compopt -D; compopt -E`},
	{"-E before -I still edits -E", `f(){ :; }; complete -I -F f; complete -E -F f; compopt -o nospace -E -I; echo "rc=$?"; compopt -I; compopt -E`},

	// And a catch-all overrides the names given beside it: the name is
	// left exactly as it was.
	{"a catch-all ignores the names", `f(){ :; }; complete -D -F f; complete -F f foo; compopt -o nospace -D foo; echo "rc=$?"; compopt -D; complete -p foo`},

	// The two statuses koi answered 0 for. The message is compared
	// separately; here it is the status a caller branches on.
	{"a name with no spec at all", `compopt -o nospace bar; echo "rc=$?"`},
	{"outside a completion function", `compopt -o nospace; echo "rc=$?"`},
	{"no arguments at all", `compopt; echo "rc=$?"`},
}

func TestCompoptEditsSpecsLikeBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	for _, tc := range compoptCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want, _ := shellRows(t, bash, dir, tc.script)
			got, _ := shellRows(t, koi, dir, tc.script)
			if !slices.Equal(got, want) {
				t.Errorf("%s\n  koi:  %q\n  bash: %q", tc.script, got, want)
			}
		})
	}
}

// compoptDiagnostics are the two answers compopt gave as a silent 0, and
// the wording is asserted in *both* shells so it is bash's because bash
// printed it here rather than because this file believes it does.
var compoptDiagnostics = []struct{ name, script, message string }{
	{"outside a completion function", `compopt -o nospace`, "compopt: not currently executing completion function"},
	{"with no arguments at all", `compopt`, "compopt: not currently executing completion function"},
	{"a name with no spec", `compopt -o nospace bar`, "compopt: bar: no completion specification"},

	// The catch-alls are named in a diagnostic by bash's internal
	// placeholder command name rather than by the option that selects
	// them, which is copied rather than replaced: it is the observable
	// answer, and a spelling of koi's own would be a divergence with
	// nothing behind it.
	{"the default spec when there is none", `compopt -o nospace -D`, "compopt: _DefaultCmD_: no completion specification"},
	{"the empty-line spec when there is none", `compopt -o nospace -E`, "compopt: _EmptycmD_: no completion specification"},
	{"the initial-word spec when there is none", `compopt -o nospace -I`, "compopt: _InitialWorD_: no completion specification"},
}

func TestCompoptDiagnosticsMatchBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	for _, tc := range compoptDiagnostics {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			script := tc.script + `; echo "rc=$?"`
			gotOut, _ := runShell(t, koi, script)
			wantOut, _ := runShell(t, bash, script)
			if wantRC := grepLine(wantOut, "rc="); wantRC != "rc=1" {
				t.Skipf("bash here answers %q for %q (%s): %q",
					wantRC, tc.script, bashVersion(t, bash), wantOut)
			}
			if got := grepLine(gotOut, "rc="); got != "rc=1" {
				t.Errorf("%s: koi %s, bash rc=1\n  koi: %q", tc.script, got, gotOut)
			}
			if !strings.Contains(wantOut, tc.message) {
				t.Errorf("%s: bash does not say %q — the expected message is invented: %q",
					tc.script, tc.message, wantOut)
			}
			if !strings.Contains(gotOut, tc.message) {
				t.Errorf("%s: koi does not say %q: %q", tc.script, tc.message, gotOut)
			}
		})
	}
}
