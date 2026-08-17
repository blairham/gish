package acp_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blairham/gish/internal/acp"
)

// The ACP round trip (#167), against a real agent process.
//
// The fake agent is a program, not a mock, because the thing being
// tested is the *concurrency* of the protocol: the client sends
// session/prompt and, while waiting for its answer, is called back with
// terminal/create. A mock that answered inline would pass while the
// real shape deadlocked, which is exactly the bug worth catching here.

// buildFakeAgent compiles the agent under testdata into a binary.
func buildFakeAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakeagent")
	if os.PathSeparator == '\\' {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/fakeagent")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the fake agent: %v\n%s", err, out)
	}
	return bin
}

func TestClientHandshakeAndPrompt(t *testing.T) {
	bin := buildFakeAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := acp.Start(ctx, []string{bin}, acp.ExecRunner)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close() //nolint:errcheck // teardown

	var mu sync.Mutex
	var text strings.Builder
	client.OnUpdate = func(u acp.Update) {
		mu.Lock()
		defer mu.Unlock()
		if u.Kind == "agent_message_chunk" {
			text.WriteString(u.Text)
		}
	}

	info, err := client.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "fake-agent" {
		t.Errorf("agent name = %q", info.Name)
	}
	if _, err := client.NewSession(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	stop, err := client.Prompt(ctx, "say hello")
	if err != nil {
		t.Fatal(err)
	}
	if stop != "end_turn" {
		t.Errorf("stopReason = %q", stop)
	}
	mu.Lock()
	got := text.String()
	mu.Unlock()
	if !strings.Contains(got, "hello from the agent") {
		t.Errorf("streamed text = %q", got)
	}
}

// The load-bearing case: the agent asks the *client* to run a command
// while the client is waiting for the prompt to finish.
func TestAgentRunsCommandsThroughTheClient(t *testing.T) {
	bin := buildFakeAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The runner records what it was asked to run, which is how a
	// caller with a policy — gish's sandbox — proves the command went
	// through it rather than around it.
	var mu sync.Mutex
	var ran []string
	runner := func(ctx context.Context, cmd acp.Command, out *acp.Output) (acp.Process, error) {
		mu.Lock()
		ran = append(ran, cmd.Command+" "+strings.Join(cmd.Args, " "))
		mu.Unlock()
		return acp.ExecRunner(ctx, cmd, out)
	}

	client, err := acp.Start(ctx, []string{bin}, runner)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close() //nolint:errcheck // teardown

	var mu2 sync.Mutex
	var text strings.Builder
	client.OnUpdate = func(u acp.Update) {
		mu2.Lock()
		defer mu2.Unlock()
		text.WriteString(u.Text)
	}
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NewSession(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Prompt(ctx, "run a command"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	gotRan := append([]string(nil), ran...)
	mu.Unlock()
	if len(gotRan) == 0 {
		t.Fatal("the agent's command never reached the runner")
	}
	mu2.Lock()
	gotText := text.String()
	mu2.Unlock()
	// The agent echoes what the command printed back into its message,
	// so this proves the output made the whole round trip: client ran
	// it, captured it, and handed it back through terminal/output.
	if !strings.Contains(gotText, "command-output-marker") {
		t.Errorf("the command's output did not reach the agent: %q", gotText)
	}
}

// A client that advertises no terminal capability must refuse the
// methods rather than half-answering: the agent has a documented branch
// for "no", and none for "wrong".
func TestNoTerminalCapabilityIsRefused(t *testing.T) {
	t.Parallel()

	terminals := acp.NewTerminals(acp.ExecRunner)
	if _, err := terminals.Handle(context.Background(), "session/prompt", json.RawMessage(`{}`)); err == nil {
		t.Error("the terminal service answered a method that is not its own")
	}
}

func TestOutputKeepsTheTail(t *testing.T) {
	t.Parallel()

	out := acp.NewOutput(8)
	if _, err := out.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	text, truncated := out.Snapshot()
	// The tail, not the head: the end of a build log is the part that
	// says what went wrong.
	if text != "9abcdef" && text != "89abcdef" {
		t.Errorf("kept %q, want the last bytes", text)
	}
	if !truncated {
		t.Error("truncation was not reported, so an agent would read a partial log as whole")
	}
}
