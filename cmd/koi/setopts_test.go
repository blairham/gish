//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetOListingMatchesBash (#245).
//
// `set -o` is a listing scripts grep and people read, and koi answered it
// with ten entries where bash answers with twenty-seven — so a probe for an
// option koi had simply never heard of read as "bash does not have it
// either". Compared whole rather than by name, because the column bash pads
// to is part of the answer for anything cutting fields out of it.
func TestSetOListingMatchesBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()

	// The column bash pads to is not stable across its own majors, and the
	// runner that matters here ships bash 3.2 — so the padding is only
	// asserted where it can be, and the names and states everywhere. The
	// same split ulimit's grammar test already makes, for the same reason.
	exactColumns := bashMajor(t, bash) >= 4

	for _, script := range []string{"set -o", "set +o"} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			got, _ := shellRows(t, koi, dir, script)
			want, _ := shellRows(t, bash, dir, script)
			if !exactColumns {
				for i := range got {
					got[i] = squeeze(got[i])
				}
				for i := range want {
					want[i] = squeeze(want[i])
				}
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s:\n koi: %q\nbash: %q", script, got, want)
			}
		})
	}
}

// TestSetRefusesWhatItCannotDo is the other half, and the one that keeps
// this honest.
//
// An option koi does not implement is accepted when it is asked for the
// state it is already in, because nothing needs to change for that to be
// true. Asked for the other state it refuses, and that refusal is the
// point: a shell that accepted an option and carried on not being in
// that mode would be the failure this issue was opened about — a header
// that produces no behavior — rather than a fix for it.
//
// `set -o posix` used to be the example here and is implemented now
// (#395), which is the outcome the rule is aimed at: the way off this
// list is to do the thing.
func TestSetRefusesWhatItCannotDo(t *testing.T) {
	t.Parallel()
	koi := buildKoi(t)
	dir := t.TempDir()

	// `set -m` used to be the first row here and is implemented now
	// (#397), and `set -H` was the second until #559 gave a script the
	// history expansion its line editor already did — which is the
	// outcome the rule is aimed at, three times over.
	for _, tc := range []struct{ script, want string }{
		{"set -v", "verbose"},
		{"set +h", "hashall"},
	} {
		t.Run(tc.script, func(t *testing.T) {
			t.Parallel()
			out, status := shellCombined(t, koi, dir, tc.script)
			if status == 0 {
				t.Errorf("%s: exit status 0 for something that did not happen", tc.script)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("%s: the refusal does not name the option: %q", tc.script, out)
			}
			// A failed builtin does not stop a script on its own -- bash
			// carries on past its own `set -Z` too -- so the check that
			// matters is that a script which asked to stop on failure
			// does stop, rather than running on with an option it
			// believes is set.
			out, status = shellCombined(t, koi, dir, "set -e; "+tc.script+"; echo REACHED")
			if status == 0 || strings.Contains(out, "REACHED") {
				t.Errorf("%s under set -e: carried on, status %d: %q", tc.script, status, out)
			}
		})
	}
}

// TestSetPhysicalResolvesSymlinks pins the half of `-o physical` koi does
// implement. bash is the oracle, and the symlink is real rather than
// mocked, because the whole question is what the kernel says the path is.
func TestSetPhysicalResolvesSymlinks(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	for _, script := range []string{
		`cd link; pwd`,                     // logical, the default
		`cd link; set -o physical; pwd`,    // resolved
		`cd link; set -o physical; pwd -L`, // -L still overrides
	} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			got, _ := shellRows(t, koi, dir, script)
			want, _ := shellRows(t, bash, dir, script)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s: koi %q, bash %q", script, got, want)
			}
		})
	}
}

// shellCombined is shellRows with stderr kept, since a refusal is the thing
// being asserted and refusals do not go to stdout.
func shellCombined(t *testing.T, shell, dir, script string) (string, int) {
	t.Helper()
	cmd := exec.Command(shell, "-c", script)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb"}
	out, err := cmd.CombinedOutput()
	status := 0
	var exitErr *exec.ExitError
	if err != nil {
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		status = exitErr.ExitCode()
	}
	return string(out), status
}
