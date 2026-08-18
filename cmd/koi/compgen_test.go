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
func shellLines(t *testing.T, shell, dir, script string) ([]string, int) {
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
	sort.Strings(lines)
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
// bash has coproc and koi does not implement it (#287), so answering
// with it would be a lie about this shell. Everything else must match,
// in both directions — a keyword bash lists and koi does not is the bug
// the issue reported, and one koi lists and bash does not would mean
// inventing a keyword.
func TestCompgenKeywordsMatchBashExceptCoproc(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	got, _ := shellLines(t, koi, dir, "compgen -k")
	want, _ := shellLines(t, bash, dir, "compgen -k")

	missing := difference(want, got)
	if strings.Join(missing, " ") != "coproc" {
		t.Errorf("keywords bash lists and koi does not = %q, want only [coproc]", missing)
	}
	if extra := difference(got, want); len(extra) > 0 {
		t.Errorf("koi lists keywords bash does not: %q", extra)
	}

	// The six that were missing all work, which is why the list was a
	// reporting bug and not an honest refusal. coproc is checked the
	// other way: it stays off the list for exactly as long as it fails.
	for _, tc := range []struct{ name, script string }{
		{"[[", "[[ 1 == 1 ]] && echo ok"},
		{"{", "{ echo ok; }"},
		{"!", "! false && echo ok"},
		{"in", "for i in ok; do echo $i; done"},
	} {
		out, status := shellLines(t, koi, dir, tc.script)
		if status != 0 || strings.Join(out, "") != "ok" {
			t.Errorf("koi lists %q as a keyword but %q gave %q (status %d)", tc.name, tc.script, out, status)
		}
	}
	if _, status := shellLines(t, koi, dir, "coproc c { :; }"); status == 0 {
		t.Error("coproc now runs; it should be added to the keyword list (#287)")
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
