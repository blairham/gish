package repl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/envtrust"
	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

// fakeEnvClient proposes whatever resp holds, for any cwd under forDir.
type fakeEnvClient struct {
	resp atomic.Pointer[pluginapi.EnvDiffResponse]
	// allowed records the for_dir values passed to Allow (#137), so a
	// test can assert the one-gesture flow reached the plugin.
	allowed  []string
	allowErr error
}

func (f *fakeEnvClient) EnvDiff(
	_ context.Context, _ *pluginapi.EnvDiffRequest, _ ...grpc.CallOption,
) (*pluginapi.EnvDiffResponse, error) {
	if r := f.resp.Load(); r != nil {
		return r, nil
	}
	return &pluginapi.EnvDiffResponse{}, nil
}

// Allow records the approval. A plugin with no second trust model would
// return unimplemented here; this one pretends to have one.
func (f *fakeEnvClient) Allow(
	_ context.Context, req *pluginapi.AllowRequest, _ ...grpc.CallOption,
) (*pluginapi.AllowResponse, error) {
	f.allowed = append(f.allowed, req.GetForDir())
	if f.allowErr != nil {
		return &pluginapi.AllowResponse{Recorded: false, Detail: f.allowErr.Error()}, nil
	}
	return &pluginapi.AllowResponse{Recorded: true}, nil
}

// envHarness builds an envManager over a temp trust store and a fake
// provider, plus a runner parked in a temp directory tree.
type envHarness struct {
	m       *envManager
	fake    *fakeEnvClient
	runner  *interp.Runner
	notices *strings.Builder
	proj    string // the directory proposals derive from
	away    string // a directory outside proj
}

func newEnvHarness(t *testing.T) *envHarness {
	t.Helper()
	base := t.TempDir()
	h := &envHarness{
		fake:    &fakeEnvClient{},
		runner:  newTestRunner(t),
		notices: &strings.Builder{},
		proj:    filepath.Join(base, "proj"),
		away:    filepath.Join(base, "away"),
	}
	trust, err := envtrust.Open(filepath.Join(base, "env-trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	warmed := make(chan struct{})
	close(warmed)
	h.m = &envManager{
		trust:     trust,
		notices:   h.notices,
		nextSeq:   func() uint64 { return 1 },
		warmed:    warmed,
		providers: []pluginhost.Provider[pluginapi.EnvProviderClient]{{Plugin: "fake-env", Client: h.fake}},
	}
	for _, dir := range []string{h.proj, h.away} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *envHarness) cd(t *testing.T, dir string) {
	t.Helper()
	if err := runEnvScript(t.Context(), h.runner, "cd "+quoteArg(t, dir)+"\n"); err != nil {
		t.Fatal(err)
	}
	h.m.atPrompt(t.Context(), h.runner)
}

// quoteArg shell-quotes one word — Windows paths carry backslashes,
// which an unquoted shell word would eat as escapes.
func quoteArg(t *testing.T, s string) string {
	t.Helper()
	quoted, err := syntax.Quote(s, syntax.LangBash)
	if err != nil {
		t.Fatal(err)
	}
	return quoted
}

func (h *envHarness) varValue(name string) (string, bool) {
	v, ok := h.runner.Vars[name]
	if !ok || !v.IsSet() {
		return "", false
	}
	return v.String(), true
}

func (h *envHarness) propose(set map[string]string, unset ...string) {
	h.fake.resp.Store(&pluginapi.EnvDiffResponse{ForDir: h.proj, Set: set, Unset: unset})
}

func TestEnvDiffPendsAppliesReverts(t *testing.T) {
	h := newEnvHarness(t)
	h.propose(map[string]string{"PROJ_VAR": "on", "LD_PRELOAD": "/evil.so"}, "PROJ_OLD")
	if err := runEnvScript(t.Context(), h.runner, "export PROJ_OLD=stale\n"); err != nil {
		t.Fatal(err)
	}

	// Entering the directory pends the proposal — nothing applies.
	h.cd(t, h.proj)
	if _, set := h.varValue("PROJ_VAR"); set {
		t.Fatal("untrusted proposal applied without allow")
	}
	if !strings.Contains(h.notices.String(), `plugin "fake-env" proposes 2 change(s)`) {
		t.Fatalf("notice missing or wrong (deny-listed vars must not count): %q", h.notices.String())
	}
	if h.m.pending == nil || !strings.Contains(strings.Join(h.m.pending.stripped, " "), "LD_PRELOAD") {
		t.Fatalf("deny-listed var not stripped: %+v", h.m.pending)
	}

	// Allowing applies the diff and records trust.
	if _, err := h.m.allowPending(t.Context(), h.runner); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.varValue("PROJ_VAR"); v != "on" {
		t.Fatalf("PROJ_VAR = %q after allow", v)
	}
	if _, set := h.varValue("PROJ_OLD"); set {
		t.Fatal("unset half of the diff not applied")
	}

	// Leaving the subtree reverts everything touched.
	h.cd(t, h.away)
	if _, set := h.varValue("PROJ_VAR"); set {
		t.Fatal("PROJ_VAR survived leaving the subtree")
	}
	if v, _ := h.varValue("PROJ_OLD"); v != "stale" {
		t.Fatalf("PROJ_OLD = %q, want restored %q", v, "stale")
	}

	// Re-entering the trusted directory re-applies silently.
	before := h.notices.Len()
	h.cd(t, h.proj)
	if v, _ := h.varValue("PROJ_VAR"); v != "on" {
		t.Fatalf("trusted diff not re-applied, PROJ_VAR = %q", v)
	}
	if h.notices.Len() != before {
		t.Errorf("trusted re-apply printed a notice: %q", h.notices.String()[before:])
	}
}

func TestEnvDiffChangedHashRePends(t *testing.T) {
	h := newEnvHarness(t)
	h.propose(map[string]string{"PROJ_VAR": "v1"})
	h.cd(t, h.proj)
	if _, err := h.m.allowPending(t.Context(), h.runner); err != nil {
		t.Fatal(err)
	}

	// The plugin's proposal changes (edited .envrc-equivalent).
	h.propose(map[string]string{"PROJ_VAR": "v2"})
	h.cd(t, h.away)
	before := h.notices.Len()
	h.cd(t, h.proj)
	if v, set := h.varValue("PROJ_VAR"); set {
		t.Fatalf("changed diff auto-applied: PROJ_VAR = %q", v)
	}
	if h.m.pending == nil || h.notices.Len() == before {
		t.Error("changed diff should pend again with a fresh notice")
	}
}

func TestEnvDiffRejectsNonAncestorForDir(t *testing.T) {
	h := newEnvHarness(t)
	h.fake.resp.Store(&pluginapi.EnvDiffResponse{
		ForDir: h.away, // not an ancestor of proj
		Set:    map[string]string{"PROJ_VAR": "on"},
	})
	h.cd(t, h.proj)
	if h.m.pending != nil {
		t.Errorf("non-ancestor for_dir accepted: %+v", h.m.pending)
	}
}

func TestEnvDiffDenyListOnlyProposalIsDropped(t *testing.T) {
	h := newEnvHarness(t)
	h.propose(map[string]string{"DYLD_INSERT_LIBRARIES": "/evil.dylib", "GISH_THEME": "evil"})
	h.cd(t, h.proj)
	if h.m.pending != nil || h.notices.Len() != 0 {
		t.Errorf("fully deny-listed proposal should vanish: %+v %q", h.m.pending, h.notices.String())
	}
}

func TestEnvDiffRevoke(t *testing.T) {
	h := newEnvHarness(t)
	h.propose(map[string]string{"PROJ_VAR": "on"})
	h.cd(t, h.proj)
	if _, err := h.m.allowPending(t.Context(), h.runner); err != nil {
		t.Fatal(err)
	}
	removed, err := h.m.revokeDir(t.Context(), h.runner, h.proj)
	if err != nil || !removed {
		t.Fatalf("revoke = %v, %v", removed, err)
	}
	if _, set := h.varValue("PROJ_VAR"); set {
		t.Fatal("revoke did not revert the live diff")
	}
	// Next entry pends again.
	h.cd(t, h.away)
	h.cd(t, h.proj)
	if h.m.pending == nil {
		t.Error("revoked directory should pend on re-entry")
	}
}

func TestTrustBuiltinUnavailableWithoutHost(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "gishrc")
	_, errOut, _ := runConfigScript(t, rc, "trust\n")
	if !strings.Contains(errOut, "not available") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestRequestedEnv(t *testing.T) {
	runner := newTestRunner(t)
	script := `export AWS_PROFILE=prod
export SECRET_TOKEN=hunter2
export LD_PRELOAD=/evil.so
export KUBECONFIG=/tmp/kc`
	if err := runEnvScript(t.Context(), runner, script+"\n"); err != nil {
		t.Fatal(err)
	}
	got := requestedEnv(runner, []string{"AWS_PROFILE", "SECRET_TOKEN", "LD_PRELOAD", "KUBECONFIG", "UNSET_VAR"})
	if got["AWS_PROFILE"] != "prod" || got["KUBECONFIG"] != "/tmp/kc" {
		t.Errorf("benign keys missing: %v", got)
	}
	if _, leaked := got["SECRET_TOKEN"]; leaked {
		t.Error("secret-shaped key served to a segment")
	}
	if _, leaked := got["LD_PRELOAD"]; leaked {
		t.Error("deny-listed key served to a segment")
	}
	if len(got) != 2 {
		t.Errorf("requestedEnv = %v", got)
	}
}

// The one-gesture trust flow (#137): approving in gish must also tell a
// plugin that wraps a tool with its own approval model, or the user is
// asked twice for one action.
func TestAllowNotifiesThePlugin(t *testing.T) {
	h := newEnvHarness(t)
	h.propose(map[string]string{"PROJECT": "demo"})
	h.cd(t, h.proj) // the cd moment pends it

	if _, err := h.m.allowPending(t.Context(), h.runner); err != nil {
		t.Fatalf("allow: %v", err)
	}
	if len(h.fake.allowed) != 1 || h.fake.allowed[0] != h.proj {
		t.Errorf("plugin Allow got %v, want [%s]", h.fake.allowed, h.proj)
	}
}

// A plugin that cannot record the approval must not block it: gish's
// own trust record is authoritative for gish, so the diff still
// applies — but the user is told, because the next shell may see the
// proposal again.
func TestAllowAppliesEvenWhenThePluginCannotRecord(t *testing.T) {
	h := newEnvHarness(t)
	h.fake.allowErr = errors.New("direnv: permission denied")
	h.propose(map[string]string{"PROJECT": "demo"})
	h.cd(t, h.proj)

	msg, err := h.m.allowPending(t.Context(), h.runner)
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	if !strings.Contains(msg, "did not record") {
		t.Errorf("message does not mention the failure: %q", msg)
	}
	if got := h.runner.Vars["PROJECT"].String(); got != "demo" {
		t.Errorf("diff was not applied: PROJECT=%q", got)
	}
}
