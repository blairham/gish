//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sandboxEnforced reports whether this machine can actually enforce —
// on Linux, a kernel without Landlock makes enforcement a best-effort
// no-op and the denial assertions must skip.
func sandboxEnforced(t *testing.T) bool {
	t.Helper()
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err == nil {
		return true // macOS Seatbelt
	}
	data, err := os.ReadFile("/sys/kernel/security/lsm")
	return err == nil && strings.Contains(string(data), "landlock")
}

// runSandboxed runs src through `gish -c` with TMPDIR pinned, so the
// policy's always-allowed temp carve-out is a directory the test owns.
func runSandboxed(t *testing.T, bin, tmpdir, dir, src string, extraEnv ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, "-c", src)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TMPDIR="+tmpdir)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestSandboxReadonlyProfile(t *testing.T) {
	if !sandboxEnforced(t) {
		t.Skip("no enforcement backend on this machine")
	}
	bin := buildGish(t)
	tmpdir, outside := t.TempDir(), t.TempDir()

	// Writing inside the temp carve-out succeeds.
	out, err := runSandboxed(t, bin, tmpdir, tmpdir,
		`sandbox --profile readonly -- sh -c 'echo ok > "$TMPDIR/probe"'`)
	if err != nil {
		t.Fatalf("tmp write should be allowed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tmpdir, "probe")); err != nil {
		t.Error("probe file missing despite success")
	}

	// Writing outside is denied.
	target := filepath.Join(outside, "denied")
	out, err = runSandboxed(t, bin, tmpdir, tmpdir,
		`sandbox --profile readonly -- sh -c 'echo x > `+target+`'`)
	if err == nil {
		t.Fatalf("outside write should be denied:\n%s", out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("denied write actually landed")
	}
}

func TestSandboxWorkspaceProfileAllowsCwd(t *testing.T) {
	if !sandboxEnforced(t) {
		t.Skip("no enforcement backend on this machine")
	}
	bin := buildGish(t)
	tmpdir, work := t.TempDir(), t.TempDir()

	out, err := runSandboxed(t, bin, tmpdir, work,
		`sandbox --profile workspace -- sh -c 'echo ok > ./built'`)
	if err != nil {
		t.Fatalf("cwd write should be allowed under workspace: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(work, "built")); err != nil {
		t.Error("cwd file missing despite success")
	}
}

func TestSandboxIsolatedFiltersEnv(t *testing.T) {
	if !sandboxEnforced(t) {
		t.Skip("no enforcement backend on this machine")
	}
	bin := buildGish(t)
	tmpdir := t.TempDir()

	out, err := runSandboxed(t, bin, tmpdir, tmpdir,
		`sandbox --profile isolated -- sh -c 'echo secret=${SECRET_TOKEN:-scrubbed}'`,
		"SECRET_TOKEN=hunter2")
	if err != nil {
		t.Fatalf("isolated echo failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "secret=scrubbed") {
		t.Errorf("secret leaked into the sandbox: %s", out)
	}
}

func TestSessionSandboxWrapsEveryCommand(t *testing.T) {
	if !sandboxEnforced(t) {
		t.Skip("no enforcement backend on this machine")
	}
	bin := buildGish(t)
	tmpdir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "denied")

	cmd := exec.Command(bin, "--sandbox", "readonly", "-c", `sh -c 'echo x > `+target+`'`)
	cmd.Dir = tmpdir
	cmd.Env = append(os.Environ(), "TMPDIR="+tmpdir)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("session sandbox did not deny the write:\n%s", out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("denied write actually landed")
	}

	// A sandboxed session refuses per-command profiles.
	cmd = exec.Command(bin, "--sandbox", "readonly", "-c", "sandbox --profile no-network -- true")
	cmd.Env = append(os.Environ(), "TMPDIR="+tmpdir)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "already sandboxed") {
		t.Errorf("escalation not refused: %v\n%s", err, out)
	}
}

func TestSandboxUnknownProfileFails(t *testing.T) {
	bin := buildGish(t)
	cmd := exec.Command(bin, "--sandbox", "yolo", "-c", "true")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "unknown sandbox profile") {
		t.Errorf("bad profile accepted: %v\n%s", err, out)
	}
}
