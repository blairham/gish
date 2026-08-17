package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Client is gish acting as an ACP client: it launches an agent, tells
// it what this client can do, and services the requests that come back.
//
// The capability set is the honest one. `terminal` is true because that
// is the whole point — an agent's commands run through gish's exec path
// with a sandbox profile and a deadline rather than through a raw
// `bash -c`. `fs` is false: reading and writing files on an agent's
// behalf is a different trust decision from running a command it showed
// you, and gish does not yet have a place to ask for it.

// Client drives one agent process.
type Client struct {
	conn      *Conn
	cmd       *exec.Cmd
	terminals *Terminals

	mu      sync.Mutex
	session string

	// OnUpdate receives session/update notifications as they arrive, so
	// a caller can stream an agent's answer instead of waiting for the
	// turn to end.
	OnUpdate func(update Update)
}

// Update is the part of a session/update notification callers act on.
type Update struct {
	Kind    string // sessionUpdate: agent_message_chunk, tool_call, plan, …
	Text    string // the text of a message chunk, when there is one
	Title   string // a tool call's title
	Status  string // a tool call's status
	Raw     json.RawMessage
	Session string
}

// AgentInfo is what the agent said about itself at initialize.
type AgentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Start launches the agent and performs the ACP handshake.
//
// runner executes the commands the agent asks for; passing nil declines
// the terminal capability outright, which is the correct thing for a
// caller that only wants text back.
func Start(ctx context.Context, argv []string, runner Runner) (*Client, error) {
	if len(argv) == 0 {
		return nil, errors.New("acp: no agent command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// The agent's stderr is its own diagnostics; it is not protocol, and
	// mixing it into the wire is how a JSON-RPC stream gets corrupted by
	// a warning.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	c := &Client{cmd: cmd}
	if runner != nil {
		c.terminals = NewTerminals(runner)
	}
	c.conn = NewConn(stdout, stdin, c.handle)
	c.conn.OnNotification(c.notified)
	go c.conn.Serve(ctx) //nolint:errcheck // the error surfaces on the next Call

	return c, nil
}

// handle answers the agent's requests.
func (c *Client) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if c.terminals != nil && strings.HasPrefix(method, "terminal/") {
		return c.terminals.Handle(ctx, method, params)
	}
	// Everything else is declined by name. A client that half-answers a
	// capability it did not advertise is worse than one that says no:
	// the agent has a documented branch for "no", and none for "wrong".
	return nil, &Error{Code: CodeMethodNotFound, Message: "method not supported: " + method}
}

// notified turns one session/update into the Update a caller sees. It
// runs on the reader goroutine, so OnUpdate must not block.
func (c *Client) notified(msg Message) {
	func() {
		if msg.Method != "session/update" || c.OnUpdate == nil {
			return
		}
		var payload struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				SessionUpdate string          `json:"sessionUpdate"`
				Content       json.RawMessage `json:"content"`
				Title         string          `json:"title"`
				Status        string          `json:"status"`
			} `json:"update"`
		}
		if err := json.Unmarshal(msg.Params, &payload); err != nil {
			return
		}
		update := Update{
			Kind:    payload.Update.SessionUpdate,
			Title:   payload.Update.Title,
			Status:  payload.Update.Status,
			Raw:     msg.Params,
			Session: payload.SessionID,
		}
		if len(payload.Update.Content) > 0 {
			var content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(payload.Update.Content, &content) == nil {
				update.Text = content.Text
			}
		}
		c.OnUpdate(update)
	}()
}

// Initialize performs the handshake and returns what the agent is.
func (c *Client) Initialize(ctx context.Context) (AgentInfo, error) {
	capabilities := map[string]any{
		// No fs: reading and writing files for an agent is a different
		// trust decision from running a command, and gish has nowhere
		// to ask for it yet.
		"fs": map[string]any{"readTextFile": false, "writeTextFile": false},
	}
	capabilities["terminal"] = c.terminals != nil

	var resp struct {
		ProtocolVersion int       `json:"protocolVersion"`
		AgentInfo       AgentInfo `json:"agentInfo"`
	}
	err := c.conn.Call(ctx, "initialize", map[string]any{
		"protocolVersion":    ProtocolVersion,
		"clientCapabilities": capabilities,
		"clientInfo":         map[string]any{"name": "gish", "version": "1"},
	}, &resp)
	if err != nil {
		return AgentInfo{}, err
	}
	if resp.ProtocolVersion != ProtocolVersion {
		// v2 is a draft whose own announcement says adding it must not
		// mean dropping v1, so an agent answering with another version
		// is a fact to report rather than a failure to hide.
		return resp.AgentInfo, &Error{
			Code:    CodeInternal,
			Message: "agent speaks ACP v" + itoa(resp.ProtocolVersion) + "; gish speaks v" + itoa(ProtocolVersion),
		}
	}
	return resp.AgentInfo, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// NewSession opens a session rooted at cwd.
func (c *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	var resp struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.conn.Call(ctx, "session/new", map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
	}, &resp); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.session = resp.SessionID
	c.mu.Unlock()
	return resp.SessionID, nil
}

// Prompt sends one turn and returns why the agent stopped.
func (c *Client) Prompt(ctx context.Context, text string) (stopReason string, err error) {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == "" {
		return "", errors.New("acp: no session")
	}
	var resp struct {
		StopReason string `json:"stopReason"`
	}
	err = c.conn.Call(ctx, "session/prompt", map[string]any{
		"sessionId": session,
		"prompt":    []any{map[string]any{"type": "text", "text": text}},
	}, &resp)
	return resp.StopReason, err
}

// Cancel asks the agent to stop the current turn — the protocol's own
// interrupt, which is what Ctrl-C has to reach.
func (c *Client) Cancel() error {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == "" {
		return nil
	}
	return c.conn.Notify("session/cancel", map[string]any{"sessionId": session})
}

// Close ends the agent and frees every terminal it left behind.
func (c *Client) Close() error {
	c.conn.Close()
	if c.terminals != nil {
		c.terminals.ReleaseAll()
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}
