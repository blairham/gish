//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBashLongOptions pins #531: the long options a caller passes to get
// a particular kind of shell. Rejecting one used to cost the whole
// invocation — a usage dump and no shell at all — which is #217's rule
// again, that argv is a contract other programs write.
func TestBashLongOptions(t *testing.T) {
	t.Parallel()

	koi := buildKoi(t)

	t.Run("--norc skips the rc file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rc := filepath.Join(dir, "koirc")
		if err := os.WriteFile(rc, []byte("echo FROM-RC\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env := append(os.Environ(), "KOI_RC="+rc)

		// -i sources the rc, which is what makes the skip observable.
		with := exec.Command(koi, "-i", "-c", "echo ran")
		with.Env = env
		out, err := with.CombinedOutput()
		if err != nil {
			t.Fatalf("koi failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "FROM-RC") {
			t.Fatalf("the rc did not run, so the skip below proves nothing: %q", out)
		}

		without := exec.Command(koi, "--norc", "-i", "-c", "echo ran")
		without.Env = env
		out, err = without.CombinedOutput()
		if err != nil {
			t.Fatalf("koi failed: %v\n%s", err, out)
		}
		if strings.Contains(string(out), "FROM-RC") {
			t.Errorf("--norc still read the rc: %q", out)
		}
		if !strings.Contains(string(out), "ran") {
			t.Errorf("--norc did not run the command: %q", out)
		}
	})

	t.Run("accepted and harmless", func(t *testing.T) {
		t.Parallel()
		for _, opt := range []string{"--noprofile", "--noediting"} {
			out, err := exec.Command(koi, opt, "-c", "echo ok").CombinedOutput()
			if err != nil {
				t.Errorf("%s: koi failed: %v\n%s", opt, err, out)
			}
			if strings.TrimSpace(string(out)) != "ok" {
				t.Errorf("%s: %q, want ok", opt, out)
			}
		}
	})

	t.Run("--rcfile is bash's spelling of --rc", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rc := filepath.Join(dir, "other")
		if err := os.WriteFile(rc, []byte("echo FROM-OTHER\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(koi, "--rcfile", rc, "-i", "-c", "echo ran").CombinedOutput()
		if err != nil {
			t.Fatalf("koi failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "FROM-OTHER") {
			t.Errorf("--rcfile did not read the named file: %q", out)
		}
	})

	t.Run("--pretty-print prints and runs nothing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		script := filepath.Join(dir, "s.sh")
		if err := os.WriteFile(script, []byte("f() {\n  echo hi\n}\nif true; then f; fi\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(koi, "--pretty-print", script).CombinedOutput()
		if err != nil {
			t.Fatalf("koi failed: %v\n%s", err, out)
		}
		got := string(out)
		if strings.Contains(got, "hi\n") && !strings.Contains(got, "echo hi") {
			t.Errorf("--pretty-print ran the script: %q", got)
		}
		// The layout is bash's, which is also declare -f's (#386).
		want := "f () \n{ \n    echo hi\n}\nif true; then\n    f;\nfi\n\n"
		if got != want {
			t.Errorf("--pretty-print output:\n%q\nwant:\n%q", got, want)
		}
	})
}
