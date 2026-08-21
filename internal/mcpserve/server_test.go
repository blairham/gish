package mcpserve

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blairham/koi-shell/internal/shell/syntax"

	"github.com/blairham/koi-shell/internal/acp"
	"github.com/blairham/koi-shell/internal/history"
	"github.com/blairham/koi-shell/internal/shell/interp"
)

// dialServer starts a server on a socket and returns a JSON-RPC client
// connection to it. The client side is the same acp.Conn the server
// uses — MCP is JSON-RPC over newline frames on both ends.
func dialServer(t *testing.T, srv *Server) *acp.Conn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(srv.Close)
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := acp.NewConn(conn, conn, nil)
	go c.Serve(context.Background()) //nolint:errcheck // ends with the conn
	return c
}

// sessionSnapshot builds a Snapshot from a real interpreter run, so the
// accessors this feature added (Aliases, DirStack, Options) are covered
// by the same test that reads them back over the wire.
func sessionSnapshot(t *testing.T) *Snapshot {
	t.Helper()
	dir := t.TempDir()
	src := `alias ll='ls -l'
f() { echo hi; }
set -e
cd .
`
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "t")
	if err != nil {
		t.Fatal(err)
	}
	r, err := interp.New(interp.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	return &Snapshot{
		Cwd:       r.Dir,
		DirStack:  r.DirStack(),
		Aliases:   r.Aliases(),
		Functions: r.Funcs,
		Options:   r.Options(),
	}
}

// callTool invokes tools/call and returns the text content plus the
// isError flag.
func callTool(t *testing.T, c *acp.Conn, name string, args any) (string, bool) {
	t.Helper()
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, "tools/call", params, &res); err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("tools/call %s content = %+v, want one text block", name, res.Content)
	}
	return res.Content[0].Text, res.IsError
}

func TestServerAnswersMCP(t *testing.T) {
	t.Parallel()
	srv := New(func(query string, n int) []history.Entry {
		if query != "make" || n != 2 {
			t.Errorf("search called with (%q, %d)", query, n)
		}
		return []history.Entry{{Command: "make build", ExitCode: 0, Cwd: "/w"}}
	}, "test-1")

	c := dialServer(t, srv)

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	ctx := context.Background()
	err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18"}, &init)
	if err != nil {
		t.Fatal(err)
	}
	if init.ServerInfo.Name != "koi" || init.ServerInfo.Version != "test-1" || init.ProtocolVersion != "2025-06-18" {
		t.Fatalf("initialize = %+v", init)
	}

	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := c.Call(ctx, "tools/list", map[string]any{}, &tools); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	want := "aliases functions options jobs cwd history_search"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("tools = %q, want %q", got, want)
	}

	// Before any prompt there is no state, and the tool says so rather
	// than answering from nothing.
	if text, isErr := callTool(t, c, "aliases", nil); !isErr || !strings.Contains(text, "prompt") {
		t.Fatalf("pre-snapshot aliases = (%q, %v), want an isError explanation", text, isErr)
	}
	// History is live, not snapshotted, so it answers even pre-prompt.
	if text, isErr := callTool(t, c, "history_search", map[string]any{"query": "make", "limit": 2}); isErr || !strings.Contains(text, "make build") {
		t.Fatalf("history_search = (%q, %v)", text, isErr)
	}

	srv.Update(sessionSnapshot(t))

	text, isErr := callTool(t, c, "aliases", nil)
	var aliases map[string]string
	if isErr || json.Unmarshal([]byte(text), &aliases) != nil || aliases["ll"] != "ls -l" {
		t.Fatalf("aliases = (%q, %v), want ll's replacement text", text, isErr)
	}
	if text, _ := callTool(t, c, "functions", nil); !strings.Contains(text, `"f"`) {
		t.Fatalf("functions list = %q, want f", text)
	}
	if text, _ := callTool(t, c, "functions", map[string]any{"name": "f"}); !strings.Contains(text, "echo hi") {
		t.Fatalf("function body = %q, want the printed source", text)
	}
	if text, isErr := callTool(t, c, "functions", map[string]any{"name": "nope"}); !isErr {
		t.Fatalf("missing function = (%q, %v), want isError", text, isErr)
	}
	text, _ = callTool(t, c, "options", nil)
	var opts map[string]bool
	if json.Unmarshal([]byte(text), &opts) != nil || !opts["errexit"] {
		t.Fatalf("options = %q, want errexit true after set -e", text)
	}
	if text, _ := callTool(t, c, "cwd", nil); !strings.Contains(text, `"dirstack"`) {
		t.Fatalf("cwd = %q", text)
	}
	if text, _ := callTool(t, c, "jobs", nil); text != "null" && !strings.HasPrefix(text, "[") {
		t.Fatalf("jobs = %q", text)
	}

	// A tool nobody defined is a protocol error, not a text answer.
	if err := c.Call(ctx, "tools/call", map[string]any{"name": "exec"}, &struct{}{}); err == nil {
		t.Fatal("unknown tool did not error")
	}
	// So is an unknown method.
	if err := c.Call(ctx, "made/up", map[string]any{}, &struct{}{}); err == nil {
		t.Fatal("unknown method did not error")
	}
}

func TestFindSocketPrefersEnvAndPrunesDead(t *testing.T) {
	// Not t.TempDir(): its path carries the full test name, and on macOS
	// sun_path caps at 104 bytes — the bind fails as "invalid argument",
	// which is exactly why SocketDir keeps production paths short.
	runtime, err := os.MkdirTemp("", "koimcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtime) })
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	t.Setenv("KOI_MCP_SOCKET", "/explicit/path.sock")
	got, err := FindSocket()
	if err != nil || got != "/explicit/path.sock" {
		t.Fatalf("FindSocket with env = (%q, %v), want the instruction honored", got, err)
	}
	t.Setenv("KOI_MCP_SOCKET", "")

	dir := filepath.Join(runtime, "koi")
	live := filepath.Join(dir, "mcp-1.sock")
	ln, err := Listen(live)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// A dead session's socket, newer than the live one so mtime order
	// alone would pick it — only the dial-check saves the proxy from a
	// decoy.
	dead := filepath.Join(dir, "mcp-2.sock")
	if err := os.WriteFile(dead, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(dead, future, future); err != nil {
		t.Fatal(err)
	}

	got, err = FindSocket()
	if err != nil || got != live {
		t.Fatalf("FindSocket = (%q, %v), want the live socket %q", got, err, live)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Error("dead socket was not pruned")
	}
}

func TestProxyCopiesBothWaysAndDrains(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "p.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Answer only after the client side has fully closed: the drain
		// half of Proxy is what this asserts.
		buf := make([]byte, 1024)
		var got []byte
		for {
			n, rerr := conn.Read(buf)
			got = append(got, buf[:n]...)
			if rerr != nil {
				break
			}
		}
		conn.Write(append([]byte("pong: "), got...)) //nolint:errcheck // test server
	}()

	var out strings.Builder
	if err := Proxy(path, strings.NewReader("ping\n"), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "pong: ping\n" {
		t.Fatalf("proxy round trip = %q", out.String())
	}
}
