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
