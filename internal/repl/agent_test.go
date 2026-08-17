package repl

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pluginapi "github.com/blairham/gish/pkg/pluginapi/v1"
)

// planningAIClient extends the fake with a canned plan.
type planningAIClient struct {
	fakeAIClient
	plan    *pluginapi.PlanResponse
	planErr error
}

func (f *planningAIClient) Plan(
	_ context.Context, _ *pluginapi.PlanRequest, _ ...grpc.CallOption,
) (*pluginapi.PlanResponse, error) {
	return f.plan, f.planErr
}

// agentHarness wires handleAgent with scripted approvals and a spy
// executor.
type agentHarness struct {
	out      strings.Builder
	executed []string
}

func runAgent(t *testing.T, plan *pluginapi.PlanResponse, answers string, execExit int) *agentHarness {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	fake := &planningAIClient{plan: plan}
	m, runner := aiHarness(t, &fake.fakeAIClient)
	m.providers[0].Client = fake
	aiMgr = m
	t.Cleanup(func() { aiMgr = nil })

	h := &agentHarness{}
	deps := agentDeps{
		runner: runner,
		in:     strings.NewReader(answers),
		out:    &h.out,
		exec: func(line string) int {
			h.executed = append(h.executed, line)
			return execExit
		},
	}
	handleAgent(t.Context(), deps, `"do the thing"`)
	return h
}

func twoStepPlan() *pluginapi.PlanResponse {
	return &pluginapi.PlanResponse{
		Summary: "two steps",
		Steps: []*pluginapi.PlanStep{
			{Title: "announce", Command: "echo hello"},
			{Title: "tidy", Command: "rm -f scratch.txt"},
		},
	}
}

func TestAgentPlansGatesAndExecutes(t *testing.T) {
	// Approve all; the rm step (shell-judged destructive) still gates.
	h := runAgent(t, twoStepPlan(), "a\nr\n", 0)

	if !strings.Contains(h.out.String(), "plan: two steps") {
		t.Errorf("plan summary missing:\n%s", h.out.String())
	}
	// The shell marked rm destructive even though the provider did not.
	if !strings.Contains(h.out.String(), "2.⚠ tidy") {
		t.Errorf("shell-side destructive marking missing:\n%s", h.out.String())
	}
	if len(h.executed) != 2 {
		t.Fatalf("executed = %q", h.executed)
	}
	// Steps run sandbox-wrapped by default.
	if h.executed[0] != "sandbox --profile workspace -- echo hello" {
		t.Errorf("step 1 = %q", h.executed[0])
	}
	if !strings.Contains(h.out.String(), "plan complete") {
		t.Errorf("no completion notice:\n%s", h.out.String())
	}
}

func TestAgentQuitExecutesNothing(t *testing.T) {
	h := runAgent(t, twoStepPlan(), "q\n", 0)
	if len(h.executed) != 0 {
		t.Fatalf("declined plan executed steps: %q", h.executed)
	}
	if !strings.Contains(h.out.String(), "nothing executed") {
		t.Errorf("no decline notice:\n%s", h.out.String())
	}
}

func TestAgentEscalationIsExplicit(t *testing.T) {
	// Step mode: run step 1 sandboxed, escalate step 2 out of the sandbox.
	h := runAgent(t, twoStepPlan(), "s\nr\n!\n", 0)
	if len(h.executed) != 2 {
		t.Fatalf("executed = %q", h.executed)
	}
	if !strings.HasPrefix(h.executed[0], "sandbox --profile workspace -- ") {
		t.Errorf("step 1 not sandboxed: %q", h.executed[0])
	}
	if h.executed[1] != "rm -f scratch.txt" {
		t.Errorf("escalated step still wrapped: %q", h.executed[1])
	}
}

func TestAgentFailureHaltsPlan(t *testing.T) {
	h := runAgent(t, twoStepPlan(), "a\nr\n", 3)
	if len(h.executed) != 1 {
		t.Fatalf("plan continued past failure: %q", h.executed)
	}
	if !strings.Contains(h.out.String(), "failed (exit 3) — plan halted") {
		t.Errorf("no halt notice:\n%s", h.out.String())
	}
}

func TestAgentSkipAndArtifact(t *testing.T) {
	h := runAgent(t, twoStepPlan(), "s\nk\nr\n", 0)
	if len(h.executed) != 1 || !strings.Contains(h.executed[0], "rm -f scratch.txt") {
		t.Fatalf("skip did not skip: %q", h.executed)
	}

	// The plan is an artifact with recorded outcomes.
	dir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "gish", "agent")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no plan artifact: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"do the thing", "two steps", "- step 1: skipped", "- step 2: ok"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("artifact missing %q:\n%s", want, data)
		}
	}
}

func TestStepDestructive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		command string
		want    bool
	}{
		{"echo hello", false},
		{"rm -rf build", true},
		{"git status && rm cache.db", true}, // destructive anywhere in the line
		{"ls | grep foo", false},
		{"if true; then", true}, // unparsable gates
	}
	for _, tt := range tests {
		if got := stepDestructive(tt.command); got != tt.want {
			t.Errorf("stepDestructive(%q) = %v, want %v", tt.command, got, tt.want)
		}
	}
}

func TestInteractiveChooserNilOffTTY(t *testing.T) {
	// Builders are not terminals: the huh frontend must stay out of
	// headless runs and tests.
	var out strings.Builder
	if c := interactiveChooser(strings.NewReader(""), &out); c != nil {
		t.Error("chooser built without a terminal")
	}
}

func TestAgentDecideLineFallback(t *testing.T) {
	// With no chooser the gate speaks the single-rune line protocol and
	// prints every option's key and label as the hint.
	var out strings.Builder
	deps := agentDeps{out: &out}
	scanner := bufio.NewScanner(strings.NewReader("x\nk\n"))
	answer, ok := deps.decide(scanner, "run step 1?", []chooseOption{
		{"r", "run (sandboxed)"}, {"k", "skip this step"},
	})
	if !ok || answer != "k" {
		t.Fatalf("decide = %q, %v", answer, ok)
	}
	if !strings.Contains(out.String(), "r=run (sandboxed)") || !strings.Contains(out.String(), "k=skip") {
		t.Errorf("hint missing options: %q", out.String())
	}
	// The invalid first answer was re-asked.
	if !strings.Contains(out.String(), "answer one of") {
		t.Errorf("invalid answer not re-asked: %q", out.String())
	}
}

// A plan artifact outlives the session, so it is the same class of
// exposure as history and sessions. Both halves need the gate: the task
// is whatever the user typed, and a step's command comes from a model
// that can echo back a literal it was shown.
func TestPlanArtifactRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	plan := &pluginapi.PlanResponse{
		Summary: "do the thing",
		Steps: []*pluginapi.PlanStep{
			{Title: "publish", Command: "curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456' https://x"},
		},
	}
	path := savePlanArtifact("deploy with GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz012345", plan)
	if path == "" {
		t.Fatal("no artifact written")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"ghp_abcdefghijklmnopqrstuvwxyz012345", "abcdefghijklmnopqrstuvwxyz123456"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("plan artifact carries a credential:\n%s", data)
		}
	}
	// The plan is still a usable record — redaction, not refusal.
	for _, keep := range []string{"do the thing", "publish"} {
		if !strings.Contains(string(data), keep) {
			t.Errorf("artifact lost %q, so redaction destroyed the record:\n%s", keep, data)
		}
	}
}
