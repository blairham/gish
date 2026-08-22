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

// trap in a non-interactive session (#268).
//
// The interpreter's own table (internal/shell/interp) covers the
// semantics case by case against real bash. What that cannot cover is
// the thing that was actually broken: koi intercepted `trap … DEBUG` at
// the call seam on *every* path and recorded it somewhere only the
// interactive loop reads, so a `-c` or script session accepted the trap,
// dropped it, and exited 0. Only a real koi invocation can tell that
// apart from a working one, which is why these run the binary.

func koiOut(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(buildKoi(t), args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb"}
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	// A non-zero status is not a failure here: every assertion below is
	// about what was printed. Anything else — the binary not starting —
	// is, and would otherwise read as empty output.
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("running koi %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestDebugTrapFiresInAScript(t *testing.T) {
	t.Parallel()
	got := koiOut(t, "-c", `trap 'echo D:$BASH_COMMAND' DEBUG; echo a; echo b`)
	want := "D:echo a\na\nD:echo b\nb\n"
	if got != want {
		t.Errorf("koi -c = %q, want %q", got, want)
	}
}

// The same trap through a script *file*, because -c and a file are
// different entry points and both were dropping it.
func TestDebugTrapFiresInAScriptFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.sh")
	body := "trap 'echo D:$BASH_COMMAND' DEBUG\necho a\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := koiOut(t, path)
	want := "D:echo a\na\n"
	if got != want {
		t.Errorf("koi script = %q, want %q", got, want)
	}
}

// `trap -l` and `kill -l` are the same listing in bash, and they are the
// same listing here. They are built from two separate tables — one in
// internal/shell/interp, one in internal/jobs — because the packages
// cannot share one without a third existing to hold it, so this is the
// test that keeps the duplicate honest. A shared variable neither of
// them compares could still be printed two different ways.
func TestTrapListMatchesKillList(t *testing.T) {
	t.Parallel()
	trapList := koiOut(t, "-c", "trap -l")
	killList := koiOut(t, "-c", "kill -l")
	if trapList != killList {
		t.Errorf("trap -l and kill -l disagree:\ntrap -l:\n%s\nkill -l:\n%s", trapList, killList)
	}
	if !strings.Contains(trapList, ") SIGINT") {
		t.Errorf("the signal listing is missing SIGINT:\n%s", trapList)
	}
}

// `trap -p` used to be a refusal with exit 2, so a script that saved a
// handler and restored it got nothing back and silently lost the
// handler. The round trip is the whole feature.
func TestTrapPRoundTrips(t *testing.T) {
	t.Parallel()
	got := koiOut(t, "-c", `trap 'echo cleanup' EXIT
saved=$(trap -p EXIT)
trap - EXIT
eval "$saved"
echo body`)
	want := "body\ncleanup\n"
	if got != want {
		t.Errorf("save and restore = %q, want %q", got, want)
	}
}
