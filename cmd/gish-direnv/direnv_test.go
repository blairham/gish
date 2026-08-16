//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/gish/pkg/pluginapi"
)

// These run against the user's real direnv when it is installed, and
// skip when it is not. That is deliberate: the whole value of this
// plugin is that direnv's *actual* semantics apply, and the behaviors
// this design turns on — which allow states exist, that export cannot
// tell them apart, that editing an .envrc re-blocks it — are exactly
// what a hand-written fake would encode from assumption. Writing this
// against a fake is how the design would have shipped with the wrong
// model of the tool it wraps.
//
// Everything is confined to t.TempDir(): direnv's own state lives under
// XDG_DATA_HOME, which is redirected, so no real allow-list is touched.

// direnvEnv builds a hermetic environment and a project directory with
// the given .envrc contents.
func direnvEnv(t *testing.T, envrc string) (dir string, env map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("direnv"); err != nil {
		t.Skip("direnv not installed")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	proj := filepath.Join(base, "proj")
	for _, d := range []string{home, proj} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if envrc != "" {
		if err := os.WriteFile(filepath.Join(proj, ".envrc"), []byte(envrc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return proj, map[string]string{
		"HOME":            home,
		"PATH":            os.Getenv("PATH"),
		"XDG_DATA_HOME":   filepath.Join(base, "data"),
		"XDG_CONFIG_HOME": filepath.Join(base, "cfg"),
	}
}

// Why the status check comes first. Export cannot distinguish the three
// states that need opposite responses — blocked (offer approval),
// denied (stay silent), broken .envrc (report it) — because it fails
// the same way for all of them, in prose, on stderr. status --json
// separates them with an enum.
func TestBlockedIsDistinguishableFromDenied(t *testing.T) {
	dir, env := direnvEnv(t, "export PROBE=hello\n")
	b := &bridge{}

	state, rcPath, err := b.state(t.Context(), dir, env)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if state != stateBlocked {
		t.Fatalf("state = %d, want blocked(%d)", state, stateBlocked)
	}
	// direnv resolves symlinks; compare in its namespace.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(rcPath) != resolved {
		t.Errorf("rc path %q not in %q", rcPath, resolved)
	}

	// Export does fail here — but only the status told us *why*, and
	// the user's variables never leak out of a blocked directory.
	if set, err := b.export(t.Context(), dir, env); err == nil {
		if v, ok := set["PROBE"]; ok {
			t.Errorf("blocked .envrc leaked PROBE=%q", v)
		}
	}
}

// The full one-gesture flow: blocked → allow → the variables appear.
func TestAllowThenExport(t *testing.T) {
	dir, env := direnvEnv(t, "export PROBE=hello\nexport SECOND=two\n")
	b := &bridge{}

	if err := b.allow(t.Context(), dir, env); err != nil {
		t.Fatalf("allow: %v", err)
	}
	state, _, err := b.state(t.Context(), dir, env)
	if err != nil {
		t.Fatal(err)
	}
	if state != stateAllowed {
		t.Fatalf("state after allow = %d, want allowed(%d)", state, stateAllowed)
	}

	set, err := b.export(t.Context(), dir, env)
	if err != nil {
		t.Fatal(err)
	}
	if set["PROBE"] != "hello" || set["SECOND"] != "two" {
		t.Errorf("export = %+v", set)
	}
}

// The state-divergence question from #137, answered by test rather than
// by assumption: editing the .envrc re-blocks direnv. Since gish's trust
// record keys on the diff hash, both re-prompt together.
func TestEditingEnvrcReBlocks(t *testing.T) {
	dir, env := direnvEnv(t, "export PROBE=one\n")
	b := &bridge{}

	if err := b.allow(t.Context(), dir, env); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := b.state(t.Context(), dir, env); state != stateAllowed {
		t.Fatalf("not allowed after allow: %d", state)
	}

	if err := os.WriteFile(filepath.Join(dir, ".envrc"), []byte("export PROBE=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, _, err := b.state(t.Context(), dir, env)
	if err != nil {
		t.Fatal(err)
	}
	if state != stateBlocked {
		t.Errorf("state after edit = %d, want blocked(%d) — the two trust models would drift", state, stateBlocked)
	}
}

// An explicit `direnv deny` is the user's own decision and must be
// honored silently. Re-proposing what someone refused is nagging, and
// gish is not the authority here.
func TestDeniedDirectoryProposesNothing(t *testing.T) {
	dir, env := direnvEnv(t, "export PROBE=hello\n")
	b := &bridge{}

	path, _ := b.resolve()
	cmd := exec.Command(path, "deny", dir) //nolint:gosec // resolved direnv, temp dir
	cmd.Dir, cmd.Env = dir, envSlice(env)
	if err := cmd.Run(); err != nil {
		t.Fatalf("direnv deny: %v", err)
	}

	state, _, err := b.state(t.Context(), dir, env)
	if err != nil {
		t.Fatal(err)
	}
	if state != stateDenied {
		t.Fatalf("state = %d, want denied(%d)", state, stateDenied)
	}

	resp, err := provider{bridge: b}.EnvDiff(t.Context(), &pluginapi.EnvDiffRequest{Cwd: dir, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetForDir() != "" || len(resp.GetSet()) != 0 {
		t.Errorf("denied directory produced a proposal: %+v", resp)
	}
}

// No .envrc anywhere: nothing proposed, no error.
func TestNoEnvrcProposesNothing(t *testing.T) {
	dir, env := direnvEnv(t, "")
	b := &bridge{}

	state, _, err := b.state(t.Context(), dir, env)
	if err != nil {
		t.Fatal(err)
	}
	if state != stateNoRC {
		t.Fatalf("state = %d, want noRC(%d)", state, stateNoRC)
	}

	resp, err := provider{bridge: b}.EnvDiff(t.Context(), &pluginapi.EnvDiffRequest{Cwd: dir, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetForDir() != "" {
		t.Errorf("proposed %q with no .envrc", resp.GetForDir())
	}
}

// A blocked directory returns for_dir with an empty set — the encoding
// that says "there is something here awaiting approval", so the host
// can offer the combined gesture rather than showing nothing.
func TestBlockedProposesForDirWithEmptySet(t *testing.T) {
	dir, env := direnvEnv(t, "export PROBE=hello\n")

	resp, err := provider{bridge: &bridge{}}.EnvDiff(t.Context(), &pluginapi.EnvDiffRequest{Cwd: dir, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetForDir() != dir {
		t.Errorf("for_dir = %q, want %q", resp.GetForDir(), dir)
	}
	if len(resp.GetSet()) != 0 {
		t.Errorf("blocked directory proposed values: %+v", resp.GetSet())
	}
}

// The end-to-end proposal, after approval.
func TestEnvDiffAfterAllow(t *testing.T) {
	dir, env := direnvEnv(t, "export PROBE=hello\n")
	p := provider{bridge: &bridge{}}

	if _, err := p.Allow(t.Context(), &pluginapi.AllowRequest{ForDir: dir, Env: env}); err != nil {
		t.Fatal(err)
	}
	resp, err := p.EnvDiff(t.Context(), &pluginapi.EnvDiffRequest{Cwd: dir, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSet()["PROBE"] != "hello" {
		t.Errorf("diff = %+v", resp.GetSet())
	}
	for k := range resp.GetSet() {
		if strings.HasPrefix(k, "DIRENV_") {
			t.Errorf("bookkeeping variable %s reached the proposal", k)
		}
	}
}

// A subdirectory inherits the parent's .envrc, and for_dir must name
// the directory the .envrc lives in — that is what makes gish's trust
// record cover the whole subtree, matching direnv's own semantics.
func TestForDirIsTheEnvrcDirectoryNotTheCwd(t *testing.T) {
	dir, env := direnvEnv(t, "export PROBE=hello\n")
	sub := filepath.Join(dir, "nested", "deeper")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	p := provider{bridge: &bridge{}}
	if _, err := p.Allow(t.Context(), &pluginapi.AllowRequest{ForDir: dir, Env: env}); err != nil {
		t.Fatal(err)
	}

	resp, err := p.EnvDiff(t.Context(), &pluginapi.EnvDiffRequest{Cwd: sub, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetForDir() != dir {
		t.Errorf("for_dir = %q, want the .envrc's directory %q", resp.GetForDir(), dir)
	}
	if resp.GetSet()["PROBE"] != "hello" {
		t.Errorf("subdirectory did not inherit: %+v", resp.GetSet())
	}
}

// stripBookkeeping is unit-testable without direnv.
func TestStripBookkeeping(t *testing.T) {
	got := stripBookkeeping(map[string]string{
		"DIRENV_DIFF": "blob", "DIRENV_WATCHES": "blob",
		"DIRENV_DIR": "-/x", "DIRENV_FILE": "/x/.envrc",
		"REAL": "value",
	})
	if len(got) != 1 || got["REAL"] != "value" {
		t.Errorf("stripBookkeeping = %+v, want just REAL", got)
	}
}

func TestRcDir(t *testing.T) {
	if got := rcDir("/a/b/.envrc", "/a/b/c"); got != "/a/b" {
		t.Errorf("rcDir = %q, want /a/b", got)
	}
	// An unreadable path falls back to cwd, keeping the response valid
	// rather than discarded by the host's ancestor check.
	if got := rcDir("", "/a/b/c"); got != "/a/b/c" {
		t.Errorf("rcDir fallback = %q, want the cwd", got)
	}
}

// No direnv installed is a normal state, not a failure: the plugin
// proposes nothing and the shell carries on.
func TestMissingDirenvProposesNothing(t *testing.T) {
	b := &bridge{}
	b.once.Do(func() {}) // pin the probe as resolved-absent
	b.available = false

	resp, err := provider{bridge: b}.EnvDiff(t.Context(), &pluginapi.EnvDiffRequest{Cwd: "/tmp", Env: nil})
	if err != nil {
		t.Fatalf("missing direnv produced an RPC error: %v", err)
	}
	if resp.GetForDir() != "" {
		t.Errorf("proposed %q with no direnv", resp.GetForDir())
	}
}
