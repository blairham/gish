//go:build unix

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/mcpserve"
)

// `koi mcp` (#473) is the stdio bridge an agent harness configures: it
// attaches to a session socket (KOI_MCP_SOCKET wins) and relays MCP
// both ways. This drives the built binary against a real server the
// test hosts, which covers the subcommand dispatch, the socket
// resolution, and the proxy's drain-on-EOF in one pass.
func TestKoiMCPProxiesToSessionSocket(t *testing.T) {
	koiBin := buildKoi(t)

	dir, err := os.MkdirTemp("", "koimcp") // sun_path cap; see mcpserve tests
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "mcp-1.sock")
	ln, err := mcpserve.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := mcpserve.New(nil, "e2e-test")
	go srv.Serve(ln)
	defer srv.Close()

	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n"
	cmd := exec.Command(koiBin, "mcp")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "KOI_MCP_SOCKET=" + sock}
	cmd.Stdin = strings.NewReader(req)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("koi mcp: %v (output %q)", err, out)
	}
	var res struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("response is not JSON: %q: %v", out, err)
	}
	if res.Result.ServerInfo.Name != "koi" || res.Result.ServerInfo.Version != "e2e-test" {
		t.Fatalf("initialize through the proxy = %q", out)
	}
}

// The half a unit test structurally cannot reach: that the *interactive
// loop* serves MCP at all. mcpManager.atPrompt is covered directly in
// internal/repl, and a mutation there proved that test says nothing
// about whether the loop calls it — so this drives a real interactive
// koi through a pty, turns the setting on the way a user would, and
// asks the running session a question through `koi mcp`.
func TestInteractiveSessionServesMCP(t *testing.T) {
	runtime, err := os.MkdirTemp("", "koimcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtime) })

	s := startPTY(t, ptyOptions{Env: []string{"XDG_RUNTIME_DIR=" + runtime}})
	s.waitForPrompt()
	// An alias defined in the session is state only this shell has —
	// reading it back over the socket proves the server is that session
	// rather than anything the test set up.
	s.runLine("alias e2ealias='echo from-the-session'")
	s.runLine("config mcp on")
	// The socket opens at the *next* prompt, so make one and wait for it.
	s.runProbe("printf 'resready\\n'", "resready")

	// The proxy needs the same runtime dir to find the session.
	cmd := exec.Command(buildKoi(t), "mcp")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"XDG_RUNTIME_DIR=" + runtime,
	}
	cmd.Stdin = strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aliases"}}` + "\n")
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("koi mcp against the live session: %v (%q)", err, got)
	}
	if !strings.Contains(string(got), "from-the-session") {
		t.Fatalf("live session did not report its own alias: %q", got)
	}
}

// With no session serving and no instruction, `koi mcp` fails with the
// line that says what to do — not a hang, not a stack trace.
func TestKoiMCPExplainsWhenNoSession(t *testing.T) {
	koiBin := buildKoi(t)
	dir, err := os.MkdirTemp("", "koimcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cmd := exec.Command(koiBin, "mcp")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"XDG_RUNTIME_DIR=" + dir, // empty socket dir
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("koi mcp succeeded with nothing to attach to: %q", out)
	}
	if !strings.Contains(string(out), "config mcp on") {
		t.Fatalf("error does not say the fix: %q", out)
	}
}
