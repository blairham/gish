// Package acp speaks the Agent Client Protocol (#167).
//
// ACP is JSON-RPC 2.0 over an agent subprocess's stdin/stdout, and its
// division of labor is the interesting part: **the client executes the
// commands.** An agent asks for `terminal/create`, and whoever is
// hosting it runs the process. That boundary is exactly where a shell
// belongs, and it is the reason this is core rather than a plugin — a
// plugin may never hold an exec channel (#34), and that invariant is
// worth more than the convenience.
//
// What ACP deliberately omits is what koi already has. The spec
// defines no permission model, no sandboxing, and no timeout — it tells
// agents to race a timer against `wait_for_exit` and call
// `terminal/kill`. Sandbox profiles (#21) and a deadline on every call
// are invariants here already, so hosting an agent through koi means
// its commands run under both without the agent doing anything.
//
// Wire version 1. v2 is a published draft as of July 2026 and its own
// announcement says adding v2 support must not mean dropping v1, so v1
// is what this speaks and the negotiation is explicit.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ProtocolVersion is the wire version this client speaks.
const ProtocolVersion = 1

// Message is a JSON-RPC 2.0 envelope. One type for requests, responses
// and notifications, because the wire does not distinguish them by
// shape — it distinguishes by which fields are present, and a decoder
// that pretends otherwise mis-routes the first unusual message it sees.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error formats the error with its Data when there is any. Data is
// where agents put the actual cause ("Query closed before response
// received" arrived there under a bare "Internal error"), and an error
// that drops it is undebuggable without re-implementing the handshake
// by hand (#331).
func (e *Error) Error() string {
	if len(e.Data) > 0 && string(e.Data) != "null" {
		return fmt.Sprintf("acp: %s (%d): %s", e.Message, e.Code, e.Data)
	}
	return fmt.Sprintf("acp: %s (%d)", e.Message, e.Code)
}

// Standard JSON-RPC codes, plus the one ACP adds for a refused method.
const (
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// Handler answers a request the agent makes of the client. Returning an
// error becomes a JSON-RPC error response; returning a nil result with
// no error becomes an empty object, which is what the terminal methods
// that answer nothing expect.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Conn is one JSON-RPC connection to an agent.
//
// The two directions are genuinely concurrent — the client sends
// session/prompt and, while waiting for its answer, is called back with
// terminal/create — so this reads on its own goroutine and routes by
// id. A design that read replies inline would deadlock on the first
// agent that actually used the terminal capability, which is the whole
// point of the exercise.
type Conn struct {
	out     io.Writer
	in      *bufio.Reader
	handler Handler

	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	pending map[string]chan Message

	// onNotify handles notifications *inline*, on the reader goroutine.
	//
	// Synchronously, deliberately. A caller streaming an agent's answer
	// needs "everything for this turn has arrived" to be true when the
	// turn's response comes back, and a separate pump goroutine makes
	// that a race — the response can be delivered while the last chunk
	// is still queued, which reads as a lost message. The cost is that
	// a handler must not block: it is on the path that also answers the
	// agent's terminal requests.
	onNotify func(Message)

	closed atomic.Bool
	done   chan struct{}
	err    error
}

// NewConn wires a connection to an agent's stdin/stdout. handler may be
// nil, in which case every request from the agent is refused with
// "method not found" — which is the correct answer for a client that
// advertised no capabilities.
func NewConn(in io.Reader, out io.Writer, handler Handler) *Conn {
	return &Conn{
		out:     out,
		in:      bufio.NewReaderSize(in, 64*1024),
		handler: handler,
		pending: map[string]chan Message{},
		done:    make(chan struct{}),
	}
}

// OnNotification sets the inline notification handler. It must be set
// before Serve starts, and it must not block.
func (c *Conn) OnNotification(fn func(Message)) { c.onNotify = fn }

// Serve reads until the connection ends. It returns the read error, or
// nil on a clean EOF.
func (c *Conn) Serve(ctx context.Context) error {
	defer close(c.done)
	for {
		line, err := c.in.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(ctx, line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			c.err = err
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (c *Conn) dispatch(ctx context.Context, line []byte) {
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return // an unparsable line is the agent's problem, not ours
	}
	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		go c.answer(ctx, msg) // a request: answering may take as long as the command does
	case msg.Method != "":
		if c.onNotify != nil {
			c.onNotify(msg)
		}
	default:
		c.deliver(msg)
	}
}

func (c *Conn) answer(ctx context.Context, req Message) {
	// A write failure here means the agent is gone, which the next Call
	// reports with a better message than this one could.
	if c.handler == nil {
		c.write(Message{JSONRPC: "2.0", ID: req.ID, Error: &Error{ //nolint:errcheck // see above
			Code: CodeMethodNotFound, Message: "method not supported: " + req.Method,
		}})
		return
	}
	result, err := c.handler(ctx, req.Method, req.Params)
	if err != nil {
		var rpcErr *Error
		if !errors.As(err, &rpcErr) {
			rpcErr = &Error{Code: CodeInternal, Message: err.Error()}
		}
		c.write(Message{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}) //nolint:errcheck // see above
		return
	}
	if result == nil {
		result = struct{}{}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		c.write(Message{JSONRPC: "2.0", ID: req.ID, Error: &Error{Code: CodeInternal, Message: err.Error()}}) //nolint:errcheck // see above
		return
	}
	c.write(Message{JSONRPC: "2.0", ID: req.ID, Result: encoded}) //nolint:errcheck // see above
}

func (c *Conn) deliver(msg Message) {
	if len(msg.ID) == 0 {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[string(msg.ID)]
	delete(c.pending, string(msg.ID))
	c.mu.Unlock()
	if ok {
		ch <- msg
	}
}

// Call sends a request and waits for its response.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	if c.closed.Load() {
		return errors.New("acp: connection closed")
	}
	id, err := json.Marshal(c.nextID.Add(1))
	if err != nil {
		return err
	}
	var encoded json.RawMessage
	if params != nil {
		if encoded, err = json.Marshal(params); err != nil {
			return err
		}
	}

	ch := make(chan Message, 1)
	c.mu.Lock()
	c.pending[string(id)] = ch
	c.mu.Unlock()

	if err := c.write(Message{JSONRPC: "2.0", ID: id, Method: method, Params: encoded}); err != nil {
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result == nil || len(resp.Result) == 0 {
			return nil
		}
		return json.Unmarshal(resp.Result, result)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return ctx.Err()
	case <-c.done:
		if c.err != nil {
			return c.err
		}
		return errors.New("acp: agent exited before answering " + method)
	}
}

// Notify sends a notification, which expects no answer.
func (c *Conn) Notify(method string, params any) error {
	var encoded json.RawMessage
	if params != nil {
		var err error
		if encoded, err = json.Marshal(params); err != nil {
			return err
		}
	}
	return c.write(Message{JSONRPC: "2.0", Method: method, Params: encoded})
}

func (c *Conn) write(msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.out.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// Close marks the connection finished for callers still waiting.
func (c *Conn) Close() { c.closed.Store(true) }
