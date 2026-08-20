//go:build unix

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The whole file is unix-only: an inherited signal disposition is a
// POSIX concept, and the test harness it uses is built there too.
//
// TestSignalIgnoredAtEntryIsListedAndUntrappable pins #441: a shell
// started with a signal ignored reports it, and a script cannot trap or
// reset it — POSIX's rule for a non-interactive shell, and bash's
// behavior.
//
// It runs koi as a child with the disposition already set, because the
// thing under test is what the *process* inherited: setting it from
// inside the test would be measuring koi's own handler instead.
func TestSignalIgnoredAtEntryIsListedAndUntrappable(t *testing.T) {
	t.Parallel()

	koi := buildKoi(t)
	// exec.Cmd has no "ignore this signal in the child" knob, so the
	// disposition is set by a shell in between. /bin/sh is enough: an
	// ignored disposition survives exec, which is the property the
	// whole feature rests on.
	cmd := exec.Command("/bin/sh", "-c", `trap "" INT; exec "$1" -c 'trap; trap "echo armed" INT; trap'`, "sh", koi)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("koi failed: %v\n%s", err, out)
	}
	got := string(out)
	if want := "trap -- '' SIGINT"; !strings.Contains(got, want) {
		t.Errorf("inherited ignore not listed:\n%s", got)
	}
	if strings.Contains(got, "echo armed") {
		t.Errorf("a signal ignored at entry was trapped anyway:\n%s", got)
	}
}

// TestNoIgnoredSignalIsInvented is the other direction: with nothing
// ignored, the listing says nothing and a trap arms normally.
func TestNoIgnoredSignalIsInvented(t *testing.T) {
	t.Parallel()

	koi := buildKoi(t)
	out, err := exec.Command(koi, "-c", `trap 'echo armed' INT; trap`).CombinedOutput()
	if err != nil {
		t.Fatalf("koi failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "echo armed") {
		t.Errorf("an ordinary trap did not arm:\n%s", out)
	}
}
