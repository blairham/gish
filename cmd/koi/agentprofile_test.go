//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Choosing the agent session's posture from the environment (#239).
//
// The environment is the only channel a harness that picks its shell
// through $SHELL controls: there is no argv to add --sandbox to. These
// tests run the real binary through a koi-agent symlink, because the
// thing being checked is which policy is *enforced* — a status line
// reporting the right profile while the policy did nothing would be the
// same silent failure the issue is about.

func agentLink(t *testing.T, bin string) string {
	t.Helper()
	agent := filepath.Join(t.TempDir(), "koi-agent-bash")
	if err := os.Symlink(bin, agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestAgentProfileEnvPicksTheProfile(t *testing.T) {
	// Not parallel: the env var is read by agentSandboxProfile in this
	// process.
	for _, tc := range []struct {
		name, env, want string
	}{
		{"unset falls back to the default", "", agentSandboxDefault},
		{"a named profile is taken", "readonly", "readonly"},
		// Not validated here — an unknown name has to reach
		// SetSessionSandbox so it is refused rather than quietly
		// replaced by the default.
		{"an unknown name is passed along", "nope", "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(agentProfileEnv, tc.env)
			if got := agentSandboxProfile("koi-agent-bash"); got != tc.want {
				t.Errorf("agentSandboxProfile = %q, want %q", got, tc.want)
			}
			// The env var says nothing about a session that is not the
			// agent entry point.
			if got := agentSandboxProfile("koi"); got != "" {
				t.Errorf("plain koi took the agent profile %q", got)
			}
		})
	}
}

// The env var has to change what is enforced, not just what is
// reported: readonly refuses a write in the working tree that the
// default workspace profile allows.
func TestAgentProfileEnvChangesEnforcement(t *testing.T) {
	if !sandboxEnforced(t) {
		t.Skip("no enforcement backend on this machine")
	}
	agent := agentLink(t, buildKoi(t))
	tmpdir, tree := t.TempDir(), t.TempDir()

	if out, err := runSandboxed(t, agent, tmpdir, tree, `sh -c 'echo ok > probe'`); err != nil {
		t.Fatalf("the default profile should allow a write in the tree: %v\n%s", err, out)
	}
	out, err := runSandboxed(t, agent, tmpdir, tree, `sh -c 'echo ok > denied'`,
		agentProfileEnv+"=readonly")
	if err == nil {
		t.Errorf("readonly should refuse a write in the tree:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(tree, "denied")); err == nil {
		t.Error("the write landed despite the profile")
	}
}

// An unknown profile stops the session rather than falling back, which
// would confine it differently from how it was asked to be.
func TestAgentProfileEnvRefusesAnUnknownName(t *testing.T) {
	agent := agentLink(t, buildKoi(t))
	tmpdir := t.TempDir()

	out, err := runSandboxed(t, agent, tmpdir, tmpdir, "echo ran", agentProfileEnv+"=nope")
	if err == nil {
		t.Errorf("an unknown profile should fail the session:\n%s", out)
	}
	if strings.Contains(out, "ran") {
		t.Errorf("the session ran anyway:\n%s", out)
	}
	if !strings.Contains(out, "unknown sandbox profile") {
		t.Errorf("the diagnostic should name the problem:\n%s", out)
	}
}

// The other half of #239: no profile is the right shape for a program
// that keeps state outside the tree it was pointed at, so the paths are
// added to whichever profile is in force.
func TestSandboxWritePathWidensTheProfile(t *testing.T) {
	if !sandboxEnforced(t) {
		t.Skip("no enforcement backend on this machine")
	}
	agent := agentLink(t, buildKoi(t))
	tmpdir, tree, state := t.TempDir(), t.TempDir(), t.TempDir()
	target := filepath.Join(state, "memory")

	// The bug as filed: the agent's own state directory is outside the
	// tree, so the default profile refuses it and the agent silently
	// stops remembering anything.
	out, err := runSandboxed(t, agent, tmpdir, tree, `sh -c 'echo x > `+target+`'`)
	if err == nil {
		t.Fatalf("a state dir outside the tree should start out refused:\n%s", out)
	}

	out, err = runSandboxed(t, agent, tmpdir, tree, `sh -c 'echo x > `+target+`'`,
		sandboxWriteEnv+"="+state)
	if err != nil {
		t.Fatalf("the named path should be writable: %v\n%s", err, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the write did not land: %v", err)
	}
}

// Widening is only what was named: adding a state directory to readonly
// must not hand back the working tree as well.
func TestSandboxWritePathWidensNothingElse(t *testing.T) {
	if !sandboxEnforced(t) {
		t.Skip("no enforcement backend on this machine")
	}
	agent := agentLink(t, buildKoi(t))
	tmpdir, tree, state := t.TempDir(), t.TempDir(), t.TempDir()

	out, err := runSandboxed(t, agent, tmpdir, tree,
		`sh -c 'echo x > `+filepath.Join(state, "allowed")+`'`,
		agentProfileEnv+"=readonly", sandboxWriteEnv+"="+state)
	if err != nil {
		t.Fatalf("the named path should be writable under readonly: %v\n%s", err, out)
	}
	out, err = runSandboxed(t, agent, tmpdir, tree, `sh -c 'echo x > in-tree'`,
		agentProfileEnv+"=readonly", sandboxWriteEnv+"="+state)
	if err == nil {
		t.Errorf("readonly should still refuse the working tree:\n%s", out)
	}
}

// A relative path is refused rather than resolved: the caller is a
// harness writing a settings file, and the shell's directory when a
// command runs is not what it meant.
func TestSandboxWritePathRefusesRelative(t *testing.T) {
	agent := agentLink(t, buildKoi(t))
	tmpdir := t.TempDir()

	out, err := runSandboxed(t, agent, tmpdir, tmpdir, "echo ran", sandboxWriteEnv+"=rel/path")
	if err == nil {
		t.Errorf("a relative write path should fail the session:\n%s", out)
	}
	if !strings.Contains(out, "not absolute") {
		t.Errorf("the diagnostic should say why:\n%s", out)
	}
}

// Several paths, the way a settings file lists them.
func TestSandboxWritePathTakesAList(t *testing.T) {
	if !sandboxEnforced(t) {
		t.Skip("no enforcement backend on this machine")
	}
	agent := agentLink(t, buildKoi(t))
	tmpdir, tree := t.TempDir(), t.TempDir()
	first, second := t.TempDir(), t.TempDir()

	for _, dir := range []string{first, second} {
		target := filepath.Join(dir, "probe")
		out, err := runSandboxed(t, agent, tmpdir, tree, `sh -c 'echo x > `+target+`'`,
			sandboxWriteEnv+"="+first+string(os.PathListSeparator)+second)
		if err != nil {
			t.Errorf("%s should be writable: %v\n%s", dir, err, out)
		}
	}
}
