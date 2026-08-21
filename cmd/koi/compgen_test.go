//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
	{"every -o name", `for o in bashdefault default dirnames filenames noquote nosort nospace plusdirs; do complete -o "$o" x || echo "refused -o $o"; done`},
	{"every -A action", `for a in alias arrayvar binding builtin command directory disabled enabled export file function group helptopic hostname job keyword running service setopt shopt signal stopped user variable; do compgen -A "$a" >/dev/null; [ $? = 2 ] && echo "refused -A $a"; done`},

	// The three catch-alls, -I included: bash 5.1 added it, koi records
	// it and does not act on it yet (#609), and refusing it would reject
	// a line bash takes.
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
