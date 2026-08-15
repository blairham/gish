package pluginhost_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

// buildFixture compiles the fixture plugin once per test run into a
// shared directory — the hermetic-integration rule from AGENTS.md.
var (
	fixtureOnce sync.Once
	fixtureDir  string
	fixtureErr  error
)

func pluginDir(t *testing.T) string {
	t.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gish-fixture")
		if err != nil {
			fixtureErr = err
			return
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, fixtureName()), "./testdata/fixture")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			fixtureErr = err
			t.Logf("fixture build: %s", out)
			return
		}
		fixtureDir = dir
	})
	if fixtureErr != nil {
		t.Fatalf("building fixture: %v", fixtureErr)
	}
	return fixtureDir
}

// fixtureName is the plugin's discovered name: Windows has no exec
// bit, so the binary carries (and is keyed by) its .exe extension.
func fixtureName() string {
	if runtime.GOOS == "windows" {
		return "fixture.exe"
	}
	return "fixture"
}

func newHost(t *testing.T) *pluginhost.Host {
	t.Helper()
	h := pluginhost.NewHost(pluginDir(t), pluginhost.WithBackoff(10*time.Millisecond))
	if err := h.Discover(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func TestDescribeAndCapabilities(t *testing.T) {
	h := newHost(t)
	statuses := h.Statuses(context.Background(), true)
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v", statuses)
	}
	st := statuses[0]
	if st.Name != fixtureName() || !st.Running || st.Version != "0.0.1-test" {
		t.Errorf("status = %+v", st)
	}
	if len(st.Capabilities) != 7 {
		t.Errorf("capabilities = %v", st.Capabilities)
	}
}

func TestPromptRenderWithinBudget(t *testing.T) {
	h := newHost(t)
	provs := h.PromptProviders(context.Background())
	if len(provs) != 1 {
		t.Fatalf("providers = %d", len(provs))
	}
	p := provs[0]

	segsCtx, cancel := context.WithTimeout(context.Background(), pluginhost.DescribeTimeout)
	defer cancel()
	segs, err := p.Client.Segments(segsCtx, &pluginapi.SegmentsRequest{})
	if err != nil || len(segs.GetSegments()) != 1 {
		t.Fatalf("segments = %v, %v", segs, err)
	}

	rctx, rcancel := context.WithTimeout(context.Background(), pluginhost.DefaultRenderBudget)
	defer rcancel()
	resp, err := p.Client.Render(rctx, &pluginapi.RenderRequest{SegmentId: "test", EventSeq: h.NextSeq()})
	if err != nil {
		t.Fatalf("render within budget failed: %v", err)
	}
	if resp.GetText() != "fixture-segment" {
		t.Errorf("text = %q", resp.GetText())
	}
}

func TestCompletionStream(t *testing.T) {
	h := newHost(t)
	provs := h.CompletionProviders(context.Background())
	if len(provs) != 1 {
		t.Fatalf("providers = %d", len(provs))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := provs[0].Client.Complete(ctx, &pluginapi.CompleteRequest{Line: "git", Cursor: 3})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if !batch.GetFinal() || batch.GetCandidates()[0].GetValue() != "fixture-git" {
		t.Errorf("batch = %v", batch)
	}
}

func TestHistoryBackendRoundtrip(t *testing.T) {
	h := newHost(t)
	provs := h.HistoryBackends(context.Background())
	if len(provs) != 1 {
		t.Fatalf("providers = %d", len(provs))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := provs[0].Client.Append(ctx, &pluginapi.AppendRequest{
		Entry: &pluginapi.HistoryEntry{Command: "make lint", Cwd: "/tmp"},
	})
	if err != nil || !resp.GetStored() {
		t.Fatalf("append = %v, %v", resp, err)
	}
	stream, err := provs[0].Client.Search(ctx, &pluginapi.SearchRequest{Query: "lint"})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := stream.Recv()
	if err != nil || len(batch.GetEntries()) != 1 || batch.GetEntries()[0].GetCommand() != "make lint" {
		t.Fatalf("search = %v, %v", batch, err)
	}
}

func TestCrashHealsWithBackoff(t *testing.T) {
	h := newHost(t)
	provs := h.PromptProviders(context.Background())
	if len(provs) != 1 {
		t.Fatal("no provider")
	}

	// Trigger the crash; the RPC fails and the process dies.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := provs[0].Client.Render(ctx, &pluginapi.RenderRequest{SegmentId: "crash"}); err == nil {
		t.Fatal("crash render unexpectedly succeeded")
	}

	// Immediate redemand may hit the backoff window; within a few
	// hundred ms the plugin must be relaunched and serving again.
	deadline := time.Now().Add(5 * time.Second)
	for {
		provs = h.PromptProviders(context.Background())
		if len(provs) == 1 {
			rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
			resp, err := provs[0].Client.Render(rctx, &pluginapi.RenderRequest{SegmentId: "test"})
			rcancel()
			if err == nil && resp.GetText() == "fixture-segment" {
				return // healed
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("plugin did not heal after crash")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMissingDirIsEmptyNotFatal(t *testing.T) {
	t.Parallel()
	h := pluginhost.NewHost(filepath.Join(t.TempDir(), "nope"))
	if err := h.Discover(); err != nil {
		t.Fatalf("Discover on missing dir: %v", err)
	}
	if provs := h.PromptProviders(context.Background()); len(provs) != 0 {
		t.Errorf("providers from missing dir: %d", len(provs))
	}
}

func TestNextSeqMonotonic(t *testing.T) {
	t.Parallel()
	h := pluginhost.NewHost(t.TempDir())
	a, b := h.NextSeq(), h.NextSeq()
	if b <= a {
		t.Errorf("NextSeq not monotonic: %d then %d", a, b)
	}
}

func TestThemeProviderRoundtrip(t *testing.T) {
	h := newHost(t)
	provs := h.ThemeProviders(context.Background())
	if len(provs) != 1 {
		t.Fatalf("theme providers = %d", len(provs))
	}

	ctx, cancel := context.WithTimeout(context.Background(), pluginhost.DescribeTimeout)
	defer cancel()
	themes, err := provs[0].Client.Themes(ctx, &pluginapi.ThemesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(themes.GetThemes()) != 1 || themes.GetThemes()[0].GetName() != "fixture-theme" {
		t.Fatalf("themes = %+v", themes.GetThemes())
	}

	rctx, rcancel := context.WithTimeout(context.Background(), pluginhost.DefaultRenderBudget)
	defer rcancel()
	resp, err := provs[0].Client.RenderPrompt(rctx, &pluginapi.RenderPromptRequest{
		Theme:   "fixture-theme",
		Context: &pluginapi.PromptContext{Cwd: "/tmp/x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetPrompt() != "fixture[/tmp/x]> " || resp.GetContPrompt() != "fixture| " ||
		resp.GetRprompt() != "fixture-right" {
		t.Errorf("render = %+v", resp)
	}
}

func TestEnvProviderRoundtrip(t *testing.T) {
	h := newHost(t)
	provs := h.EnvProviders(context.Background())
	if len(provs) != 1 {
		t.Fatalf("env providers = %d", len(provs))
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginhost.DefaultEnvBudget)
	defer cancel()

	// No proposal outside the fixture's directory pattern.
	resp, err := provs[0].Client.EnvDiff(ctx, &pluginapi.EnvDiffRequest{Cwd: "/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetForDir() != "" {
		t.Errorf("unexpected proposal: %+v", resp)
	}

	resp, err = provs[0].Client.EnvDiff(ctx, &pluginapi.EnvDiffRequest{Cwd: "/tmp/envdir/sub"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetForDir() != "/tmp/envdir/sub" || resp.GetSet()["FIXTURE_ENV"] != "on" ||
		len(resp.GetUnset()) != 1 {
		t.Errorf("proposal = %+v", resp)
	}
}

func TestAIProviderRoundtrip(t *testing.T) {
	h := newHost(t)
	provs := h.AIProviders(context.Background())
	if len(provs) != 1 {
		t.Fatalf("ai providers = %d", len(provs))
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginhost.DescribeTimeout)
	defer cancel()

	stream, err := provs[0].Client.Compose(ctx, &pluginapi.ComposeRequest{Query: "list files"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if first.GetCommand() != "echo composed:list files" || first.GetExplanation() != "fixture rationale" {
		t.Errorf("first candidate = %+v", first)
	}

	resp, err := provs[0].Client.Explain(ctx, &pluginapi.ExplainRequest{Command: "make test"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetExplanation() != "fixture explains: make test" {
		t.Errorf("explain = %q", resp.GetExplanation())
	}
}
