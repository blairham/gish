//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAgentSandboxProfile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		argv0 string
		want  string
	}{
		{"gish", ""},
		{"/usr/local/bin/gish", ""},
		{"-gish", ""},
		{"gish-agent", agentSandboxDefault},
		{"/Users/x/.local/bin/gish-agent", agentSandboxDefault},
		// The spelling a harness that greps $SHELL for "bash" forces.
		{"gish-agent-bash", agentSandboxDefault},
		{"-gish-agent", agentSandboxDefault},
		// Not the agent entry point: the name has to be gish-agent, or
		// gish-agent with a suffix behind a dash.
		{"my-gish-agent", ""},
		{"gish-agentless", ""},
	}
	for _, tc := range cases {
		if got := agentSandboxProfile(tc.argv0); got != tc.want {
			t.Errorf("agentSandboxProfile(%q) = %q, want %q", tc.argv0, got, tc.want)
		}
	}
}

// The name is the whole install: a symlink called gish-agent runs the
// same binary with the sandbox already on, so a harness that can only
// be handed a path still gets a confined shell.
func TestAgentNameSandboxesTheSession(t *testing.T) {
	if !sandboxEnforced(t) {
		t.Skip("no enforcement backend on this machine")
	}
	bin := buildGish(t)
	linkDir, tmpdir, outside := t.TempDir(), t.TempDir(), t.TempDir()
	agent := filepath.Join(linkDir, "gish-agent-bash")
	if err := os.Symlink(bin, agent); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "denied")

	out, err := runSandboxed(t, agent, tmpdir, tmpdir, `sh -c 'echo x > `+target+`'`)
	if err == nil {
		t.Fatalf("a gish-agent session should confine writes:\n%s", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the write landed despite the sandbox")
	}

	// Writing in the working tree is the point of the workspace profile.
	if out, err := runSandboxed(t, agent, tmpdir, tmpdir, `sh -c 'echo ok > probe'`); err != nil {
		t.Fatalf("a write in the working tree should be allowed: %v\n%s", err, out)
	}

	// The same binary under its own name confines nothing.
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if out, err := runSandboxed(t, bin, tmpdir, tmpdir, `sh -c 'echo x > `+target+`'`); err != nil {
		t.Fatalf("plain gish should not confine anything: %v\n%s", err, out)
	}

	// An explicit --sandbox always wins, including the opt-out.
	cmd := exec.Command(agent, "--sandbox", "none", "-c", `sh -c 'echo x > `+target+`'`)
	cmd.Dir = tmpdir
	cmd.Env = append(os.Environ(), "TMPDIR="+tmpdir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("--sandbox none should opt out of the agent default: %v\n%s", err, out)
	}
}
