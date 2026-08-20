// Package mcpserve serves live shell state over the Model Context
// Protocol (#473).
//
// The shape is dictated by two facts. First, a tier-2 plugin cannot be
// this: in go-plugin the host is the gRPC client and every service in
// proto/koi/plugin/v1 is host→plugin, so a plugin has no channel to ask
// the shell anything — the server has to live in the shell's own
// process, beside the state it reports. Second, an MCP client is a
// separate process the shell does not spawn, so the meeting point is a
// per-session unix socket in a 0700 directory, with `koi mcp` as the
// stdio↔socket proxy an agent harness actually configures.
//
// Read-only, structurally: no tool executes anything, so #34's "a
// plugin may never hold an exec channel" has nothing here to hold —
// execution stays with ACP's terminal serve mode, the other half of the
// agent edge (docs/acp.md). The precedent is #214's exception test: a
// read-only viewer of shell state creates pull toward koi rather than
// parity elsewhere.
//
// State is a snapshot swapped in at each prompt rather than read live:
// the Runner is not safe for concurrent use, and between prompts is
// exactly when an interactive session's state is stable. The one live
// read is history search, whose store carries its own lock.
//
// MCP is JSON-RPC 2.0 over newline-delimited frames — the same wire the
// acp package already speaks — so the transport is [acp.Conn] rather
// than a second framing layer.
package mcpserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/acp"
	"github.com/blairham/koi-shell/internal/history"
	"github.com/blairham/koi-shell/internal/jobs"
	"github.com/blairham/koi-shell/internal/shell/interp"
)

// protocolVersion is the MCP revision this server was written against.
// initialize echoes the client's requested version instead when one is
// named: the surface here (initialize, tools/list, tools/call) predates
// every published revision and has been stable across all of them.
const protocolVersion = "2025-06-18"

// Snapshot is the shell state as of the last prompt. Function bodies
// stay as parse trees and render on demand: a definition is immutable
// once made (redefinition replaces the map entry), so printing from a
// server goroutine races nothing, and most snapshots are never asked.
type Snapshot struct {
	Cwd       string
	DirStack  []string
	Aliases   map[string]string
	Functions map[string]*syntax.Stmt
	Options   []interp.OptionState
	Jobs      []jobs.JobInfo
}

// Server answers MCP over any number of concurrent client connections.
type Server struct {
	snap    atomicSnapshot
	search  func(query string, n int) []history.Entry
	version string

	mu     sync.Mutex
	ln     net.Listener
	closed bool
}

// atomicSnapshot avoids importing sync/atomic's generics dance inline.
type atomicSnapshot struct {
	mu sync.RWMutex
	s  *Snapshot
}

func (a *atomicSnapshot) load() *Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.s
}

func (a *atomicSnapshot) store(s *Snapshot) {
	a.mu.Lock()
	a.s = s
	a.mu.Unlock()
}

// New builds a server. search may be nil, in which case the
// history_search tool reports that history is unavailable rather than
// being absent — a tool that comes and goes would read as flaky.
func New(search func(query string, n int) []history.Entry, version string) *Server {
	return &Server{search: search, version: version}
}

// Update swaps in the state clients see from now on.
func (s *Server) Update(snap *Snapshot) { s.snap.store(snap) }

// Listen opens the session's socket. The parent directory is created
// 0700 — the socket is the trust boundary, and "same user only" is the
// whole policy. A stale socket at the same path (a crashed session with
// this pid's number reused) is removed rather than reported: nothing
// else can legitimately hold it.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return net.Listen("unix", path)
}

// Serve accepts clients until the listener closes. Each connection is
// its own JSON-RPC conversation; a client that hangs costs its own
// goroutine and nothing shared.
func (s *Server) Serve(ln net.Listener) {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // closed listener: the session is shutting down
		}
		go func() {
			defer conn.Close()
			c := acp.NewConn(conn, conn, s.handle)
			// Inline answers: a client that sends its request and
			// half-closes must still get the response before this
			// goroutine tears the connection down.
			c.SyncRequests()
			_ = c.Serve(context.Background())
		}()
	}
}

// Close stops accepting and removes nothing: the socket file's removal
// belongs to the caller that chose its path.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.ln != nil {
		_ = s.ln.Close()
	}
}

func (s *Server) handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var req struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &req)
		ver := req.ProtocolVersion
		if ver == "" {
			ver = protocolVersion
		}
		return map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "koi", "version": s.version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolList}, nil
	case "tools/call":
		var req struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &acp.Error{Code: acp.CodeInvalidParams, Message: err.Error()}
		}
		return s.callTool(req.Name, req.Arguments)
	default:
		return nil, &acp.Error{Code: acp.CodeMethodNotFound, Message: "method not supported: " + method}
	}
}

// toolList is what tools/list returns: name, what it answers, and a
// JSON schema for the arguments. Every tool is a question — none has a
// side effect, which is the design's one invariant.
var toolList = []map[string]any{
	{
		"name":        "aliases",
		"description": "The session's alias definitions, name to replacement text.",
		"inputSchema": emptySchema,
	},
	{
		"name":        "functions",
		"description": "Shell function names; pass a name for that function's source.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "function to print"},
			},
			"additionalProperties": false,
		},
	},
	{
		"name":        "options",
		"description": "Every supported shell option (`set -o` and `shopt` both) with its live state.",
		"inputSchema": emptySchema,
	},
	{
		"name":        "jobs",
		"description": "The live job table: id, process group, state, command line.",
		"inputSchema": emptySchema,
	},
	{
		"name":        "cwd",
		"description": "The working directory and the pushd/popd stack, top first.",
		"inputSchema": emptySchema,
	},
	{
		"name":        "history_search",
		"description": "Search session history with metadata: command, cwd, exit status, duration. Empty query returns the most recent entries. Secret-bearing commands never reach the history store, so results are safe to read.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "substring to match; empty for all"},
				"limit": map[string]any{"type": "integer", "description": "max entries, default 20"},
			},
			"additionalProperties": false,
		},
	},
}

var emptySchema = map[string]any{"type": "object", "additionalProperties": false}

// callTool answers one tool invocation. Domain errors (an unknown
// function name) come back as isError tool results per MCP, so the
// agent sees them as an answer rather than a broken server.
func (s *Server) callTool(name string, args json.RawMessage) (any, error) {
	snap := s.snap.load()
	if snap == nil && name != "history_search" {
		return toolError("the session has not reached a prompt yet"), nil
	}
	switch name {
	case "aliases":
		return toolJSON(snap.Aliases)
	case "functions":
		var req struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(args, &req)
		if req.Name == "" {
			names := make([]string, 0, len(snap.Functions))
			for fn := range snap.Functions {
				names = append(names, fn)
			}
			sort.Strings(names)
			return toolJSON(names)
		}
		body, ok := snap.Functions[req.Name]
		if !ok {
			return toolError(fmt.Sprintf("no function named %q", req.Name)), nil
		}
		var sb strings.Builder
		if err := syntax.NewPrinter().Print(&sb, body); err != nil {
			return toolError("function body failed to print: " + err.Error()), nil
		}
		return toolText(sb.String()), nil
	case "options":
		opts := make(map[string]bool, len(snap.Options))
		for _, o := range snap.Options {
			opts[o.Name] = o.Set
		}
		return toolJSON(opts)
	case "jobs":
		return toolJSON(snap.Jobs)
	case "cwd":
		return toolJSON(map[string]any{"cwd": snap.Cwd, "dirstack": snap.DirStack})
	case "history_search":
		if s.search == nil {
			return toolError("history is unavailable in this session"), nil
		}
		var req struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		_ = json.Unmarshal(args, &req)
		if req.Limit <= 0 || req.Limit > 200 {
			req.Limit = 20
		}
		entries := s.search(req.Query, req.Limit)
		if entries == nil {
			entries = []history.Entry{}
		}
		return toolJSON(entries)
	default:
		return nil, &acp.Error{Code: acp.CodeInvalidParams, Message: "unknown tool: " + name}
	}
}

func toolText(text string) any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

func toolError(text string) any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": true,
	}
}

func toolJSON(v any) (any, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, &acp.Error{Code: acp.CodeInternal, Message: err.Error()}
	}
	return toolText(string(b)), nil
}

// SocketDir is where sessions put their MCP sockets: the runtime dir
// when the platform declares one, else a per-user directory under the
// temp root — 0700 either way, because the directory is the whole
// access policy.
func SocketDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "koi")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("koi-%d", os.Getuid()))
}

// SocketPath names this process's session socket.
func SocketPath() string {
	return filepath.Join(SocketDir(), fmt.Sprintf("mcp-%d.sock", os.Getpid()))
}

// FindSocket resolves the socket a proxy should attach to:
// KOI_MCP_SOCKET when set (an instruction, not a candidate — it wins
// whether or not anything listens there yet), else the newest live
// socket in SocketDir. Sockets that refuse a connection are from dead
// sessions and are removed in passing, so a crash does not leave a
// decoy the next proxy attaches to forever.
func FindSocket() (string, error) {
	if p := os.Getenv("KOI_MCP_SOCKET"); p != "" {
		return p, nil
	}
	matches, err := filepath.Glob(filepath.Join(SocketDir(), "mcp-*.sock"))
	if err != nil || len(matches) == 0 {
		return "", errors.New("no koi session is serving MCP — run `config mcp on` in a koi session")
	}
	slices.SortFunc(matches, func(a, b string) int {
		ai, _ := os.Stat(a)
		bi, _ := os.Stat(b)
		switch {
		case ai == nil || bi == nil:
			return 0
		default:
			return bi.ModTime().Compare(ai.ModTime()) // newest first
		}
	})
	for _, m := range matches {
		conn, err := net.Dial("unix", m)
		if err != nil {
			_ = os.Remove(m)
			continue
		}
		_ = conn.Close()
		return m, nil
	}
	return "", errors.New("no koi session is serving MCP — run `config mcp on` in a koi session")
}
