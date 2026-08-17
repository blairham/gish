package repl

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/history"
	"github.com/blairham/gish/internal/pluginhost"
	pluginapi "github.com/blairham/gish/pkg/pluginapi/v1"
)

// fakeComposeStream feeds queued candidates then EOF.
type fakeComposeStream struct {
	grpc.ClientStream
	queue []*pluginapi.ComposeCandidate
}

func (f *fakeComposeStream) Recv() (*pluginapi.ComposeCandidate, error) {
	if len(f.queue) == 0 {
		return nil, io.EOF
	}
	next := f.queue[0]
	f.queue = f.queue[1:]
	return next, nil
}

// fakeAIClient records the last requests it saw.
type fakeAIClient struct {
	candidates []*pluginapi.ComposeCandidate
	explainOut string
	err        error

	lastCompose *pluginapi.ComposeRequest
	lastExplain *pluginapi.ExplainRequest
}

func (f *fakeAIClient) Compose(
	_ context.Context, req *pluginapi.ComposeRequest, _ ...grpc.CallOption,
) (pluginapi.AIProvider_ComposeClient, error) {
	f.lastCompose = req
	if f.err != nil {
		return nil, f.err
	}
	return &fakeComposeStream{queue: f.candidates}, nil
}

func (f *fakeAIClient) Explain(
	_ context.Context, req *pluginapi.ExplainRequest, _ ...grpc.CallOption,
) (*pluginapi.ExplainResponse, error) {
	f.lastExplain = req
	if f.err != nil {
		return nil, f.err
	}
	return &pluginapi.ExplainResponse{Explanation: f.explainOut}, nil
}

func (f *fakeAIClient) Plan(
	context.Context, *pluginapi.PlanRequest, ...grpc.CallOption,
) (*pluginapi.PlanResponse, error) {
	return nil, errors.New("fakeAIClient has no plan; use planningAIClient")
}

// aiHarness builds an aiManager over a fake provider and a real
// (temp-dir) history store.
func aiHarness(t *testing.T, fake *fakeAIClient) (*aiManager, *interp.Runner) {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	warmed := make(chan struct{})
	close(warmed)
	m := &aiManager{
		nextSeq:   func() uint64 { return 1 },
		store:     store,
		warmed:    warmed,
		providers: []pluginhost.Provider[pluginapi.AIProviderClient]{{Plugin: "fake-ai", Client: fake}},
	}
	return m, newTestRunner(t)
}

func TestComposeFirstCandidateWins(t *testing.T) {
	fake := &fakeAIClient{candidates: []*pluginapi.ComposeCandidate{
		{Command: "  du -sh * | sort -h  ", Explanation: "sizes, sorted"},
		{Command: "ls -laS", Final: true},
	}}
	m, runner := aiHarness(t, fake)

	cmd, why, err := m.compose(t.Context(), runner, "biggest files here")
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "du -sh * | sort -h" || why != "sizes, sorted" {
		t.Errorf("compose = %q / %q", cmd, why)
	}
	if fake.lastCompose.GetQuery() != "biggest files here" {
		t.Errorf("query = %q", fake.lastCompose.GetQuery())
	}
	if fake.lastCompose.GetContext().GetOs() == "" || fake.lastCompose.GetContext().GetCwd() == "" {
		t.Errorf("context not assembled: %+v", fake.lastCompose.GetContext())
	}
}

func TestComposeContextIsScrubSafe(t *testing.T) {
	fake := &fakeAIClient{candidates: []*pluginapi.ComposeCandidate{{Command: "true", Final: true}}}
	m, runner := aiHarness(t, fake)

	// A secret-bearing command never reaches the store, so it can never
	// reach the provider; the clean command does.
	if _, err := m.store.Append(history.Entry{Command: "export STRIPE_KEY=sk_live_abcdefghij1234"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.Append(history.Entry{Command: "make test"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.compose(t.Context(), runner, "q"); err != nil {
		t.Fatal(err)
	}
	recent := fake.lastCompose.GetContext().GetRecentCommands()
	joined := strings.Join(recent, "\n")
	if strings.Contains(joined, "sk_live") {
		t.Fatalf("secret reached the provider: %q", recent)
	}
	if !strings.Contains(joined, "make test") {
		t.Errorf("clean history missing: %q", recent)
	}
	// The env map is the allowlist, never the full environment.
	if _, leaked := fake.lastCompose.GetContext().GetEnv()["AWS_SECRET_ACCESS_KEY"]; leaked {
		t.Error("non-allowlisted env leaked")
	}
}

func TestExplainUsesStoreNotMemory(t *testing.T) {
	fake := &fakeAIClient{explainOut: "because reasons"}
	m, runner := aiHarness(t, fake)

	if _, err := m.explain(t.Context(), runner); err == nil {
		t.Error("explain with empty history should fail, not send nothing")
	}
	if _, err := m.store.Append(history.Entry{Command: "go build ./..."}); err != nil {
		t.Fatal(err)
	}
	m.note(2)
	out, err := m.explain(t.Context(), runner)
	if err != nil || out != "because reasons" {
		t.Fatalf("explain = %q, %v", out, err)
	}
	if fake.lastExplain.GetCommand() != "go build ./..." || fake.lastExplain.GetExitCode() != 2 {
		t.Errorf("explain request = %+v", fake.lastExplain)
	}
}

func TestProviderSelection(t *testing.T) {
	fake := &fakeAIClient{candidates: []*pluginapi.ComposeCandidate{{Command: "true", Final: true}}}
	m, runner := aiHarness(t, fake)

	if err := runner.Run(t.Context(), parseLine(t, `GISH_AI_PROVIDER=other`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.compose(t.Context(), runner, "q"); err == nil ||
		!strings.Contains(err.Error(), "matches no installed plugin") {
		t.Errorf("unknown provider not rejected: %v", err)
	}
	if err := runner.Run(t.Context(), parseLine(t, `GISH_AI_PROVIDER=fake-ai`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.compose(t.Context(), runner, "q"); err != nil {
		t.Errorf("named provider rejected: %v", err)
	}
}

func TestComposePrefillSandboxWrapping(t *testing.T) {
	runner := newTestRunner(t)

	got := composePrefill(runner, "du -sh *")
	if got != "sandbox --profile workspace -- du -sh *" {
		t.Errorf("prefill = %q", got)
	}
	// Shell-state commands must not be wrapped in a child process.
	if got := composePrefill(runner, "cd /tmp"); got != "cd /tmp" {
		t.Errorf("cd wrapped: %q", got)
	}
	// The knob turns it off.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_AI_SANDBOX=off`)); err != nil {
		t.Fatal(err)
	}
	if got := composePrefill(runner, "du -sh *"); got != "du -sh *" {
		t.Errorf("off knob ignored: %q", got)
	}
}

func TestHandleComposePreloadsBuffer(t *testing.T) {
	fake := &fakeAIClient{candidates: []*pluginapi.ComposeCandidate{
		{Command: "rm -rf ./build", Explanation: "clears the build dir", Final: true},
	}}
	m, runner := aiHarness(t, fake)
	aiMgr = m
	t.Cleanup(func() { aiMgr = nil })

	var preloaded string
	var out strings.Builder
	handleCompose(t.Context(), runner, "clean build artifacts", func(s string) { preloaded = s }, &out)
	if preloaded != "sandbox --profile workspace -- rm -rf ./build" {
		t.Errorf("preloaded = %q", preloaded)
	}
	if !strings.Contains(out.String(), "clears the build dir") {
		t.Errorf("explanation not shown: %q", out.String())
	}

	// Provider failure surfaces, preloads nothing.
	preloaded = ""
	fake.err = errors.New("model unavailable")
	handleCompose(t.Context(), runner, "q", func(s string) { preloaded = s }, &out)
	if preloaded != "" || !strings.Contains(out.String(), "model unavailable") {
		t.Errorf("failure handling: preloaded=%q out=%q", preloaded, out.String())
	}
}
