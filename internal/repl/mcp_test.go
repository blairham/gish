package repl

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blairham/koi-shell/internal/shell/syntax"

	"github.com/blairham/koi-shell/internal/acp"
	"github.com/blairham/koi-shell/internal/jobs"
	"github.com/blairham/koi-shell/internal/mcpserve"
	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/interp"
)

// The MCP wiring (#473): KOI_MCP=on at a prompt opens the session
// socket and serves the runner's live state; setting it off closes and
// removes the socket. This drives the same atPrompt the interactive
// loop calls, with the runner state built by actually running commands.
func TestMCPManagerServesAndStops(t *testing.T) {
	// Not t.TempDir(): macOS caps sun_path at 104 bytes and the test
	// name would blow it — the same reason SocketDir stays short.
	runtime, err := os.MkdirTemp("", "koimcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtime) })
	t.Setenv("XDG_RUNTIME_DIR", runtime)

	runner, err := interp.New(
		interp.Dir(t.TempDir()),
		interp.Env(expand.ListEnviron("PATH="+os.Getenv("PATH"), "KOI_MCP=on")),
	)
	if err != nil {
		t.Fatal(err)
	}
	run := func(src string) {
		t.Helper()
		file, perr := syntax.NewParser().Parse(strings.NewReader(src), "t")
		if perr != nil {
			t.Fatal(perr)
		}
		if rerr := runner.Run(context.Background(), file); rerr != nil {
			t.Fatal(rerr)
		}
	}
	run("alias gs='git status'")

	m := newMCPManager(jobs.NewTable(nil))
	defer m.close()
	m.atPrompt(runner)

	sock := mcpserve.SocketPath()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("socket not served after KOI_MCP=on prompt: %v", err)
	}
	c := acp.NewConn(conn, conn, nil)
	go c.Serve(context.Background()) //nolint:errcheck // ends with the conn
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	err = c.Call(ctx, "tools/call", map[string]any{"name": "aliases"}, &res)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || !strings.Contains(res.Content[0].Text, "git status") {
		t.Fatalf("aliases over the socket = %+v, want the alias the session defined", res)
	}
	conn.Close()

	run("KOI_MCP=off")
	m.atPrompt(runner)
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket still present after KOI_MCP=off prompt")
	}
	if _, err := net.Dial("unix", sock); err == nil {
		t.Fatal("socket still accepting after KOI_MCP=off prompt")
	}
}
