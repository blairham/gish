//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The file is unix-only because buildKoi is: the harness lives in
// startup_test.go, which is tagged for the pty work it also holds.
// Nothing here is unix-specific in principle.
//
// TestInvocationOptionSurface pins the argv shapes #427 found missing:
// shopt's -O/+O form, the SHELLOPTS and BASHOPTS import, and a script
// operand found on PATH.
//
// They are end-to-end because each is about what the *process* was
// started with, which is the only place the behavior exists.
func TestInvocationOptionSurface(t *testing.T) {
	t.Parallel()

	koi := buildKoi(t)

	t.Run("-O sets a shopt before the first line", func(t *testing.T) {
		t.Parallel()
		out, err := exec.Command(koi, "-O", "nullglob", "-c", "shopt nullglob").CombinedOutput()
		if err != nil {
			t.Fatalf("koi failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "on") {
			t.Errorf("-O did not set the option: %q", out)
		}
	})

	t.Run("+O unsets one", func(t *testing.T) {
		t.Parallel()
		// The status is ignored on purpose: a bare `shopt name` answers
		// through it, so querying an option that is off is exit 1 (#393)
		// — which is precisely what this asserts.
		out, _ := exec.Command(koi, "+O", "nullglob", "-c", "shopt nullglob").CombinedOutput()
		if !strings.Contains(string(out), "off") {
			t.Errorf("+O did not leave the option off: %q", out)
		}
	})

	t.Run("SHELLOPTS is imported", func(t *testing.T) {
		t.Parallel()
		cmd := exec.Command(koi, "-c", `case $- in *f*) echo has-f;; *) echo no-f;; esac`)
		cmd.Env = append(os.Environ(), "SHELLOPTS=noglob")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("koi failed: %v\n%s", err, out)
		}
		if strings.TrimSpace(string(out)) != "has-f" {
			t.Errorf("SHELLOPTS was ignored: %q", out)
		}
	})

	t.Run("BASHOPTS is imported", func(t *testing.T) {
		t.Parallel()
		cmd := exec.Command(koi, "-c", "shopt nullglob")
		cmd.Env = append(os.Environ(), "BASHOPTS=nullglob")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("koi failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "on") {
			t.Errorf("BASHOPTS was ignored: %q", out)
		}
	})

	t.Run("an unknown imported option is skipped", func(t *testing.T) {
		t.Parallel()
		// The environment may come from a shell with options koi does
		// not have, and refusing to start over one would be worse than
		// ignoring it.
		cmd := exec.Command(koi, "-c", "echo ran")
		cmd.Env = append(os.Environ(), "SHELLOPTS=nosuchoption")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("koi failed: %v\n%s", err, out)
		}
		if strings.TrimSpace(string(out)) != "ran" {
			t.Errorf("an unknown option was not skipped quietly: %q", out)
		}
	})

	t.Run("a script operand is found on PATH", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		script := filepath.Join(dir, "koiscript")
		if err := os.WriteFile(script, []byte("echo from-path\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(koi, "koiscript")
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("koi failed: %v\n%s", err, out)
		}
		if strings.TrimSpace(string(out)) != "from-path" {
			t.Errorf("the operand was not searched for on PATH: %q", out)
		}
	})
}
