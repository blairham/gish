package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
)

// The terminal capability (#167): the half of ACP where the *client*
// executes, which is the half a shell should own.
//
// The spec defines no permission model, no sandboxing and no timeout.
// That is not a criticism — it is a protocol, and those are policy — but
// it does mean every ACP client today runs an agent's commands the way
// `bash -c` would. koi has somewhere better to put them: the same exec
// path its own commands take, wrapped in a sandbox profile (#21).
//
// So the shape here is deliberately narrow. This does not reimplement
// running a command; it hands the command to a runner supplied by the
// caller, which in koi's case is the sandboxed exec path. What lives
// here is the protocol's bookkeeping: terminal ids, a capped output
// buffer, and the exit status the agent polls for.

// OutputLimit is the default cap on retained output per terminal.
//
// The protocol lets the agent ask for a byte limit and says nothing
// about what happens when it does not. Keeping the *tail* is the right
// default for the same reason the blocks ring keeps it (#99): the end
// of a build log is the part that says what went wrong.
const OutputLimit = 1 << 20 // 1 MiB

// Command is one command an agent asked to run.
type Command struct {
	Command string
	Args    []string
	Cwd     string
	Env     []string
}

// Runner executes a command on behalf of an agent. It returns a handle
// the terminal service can poll and kill.
//
// This is the seam that keeps policy out of the protocol: koi passes a
// runner that wraps every command in a sandbox profile and refuses the
// ones its own lint rules call destructive, and a test passes one that
// records what it was asked to do.
type Runner func(ctx context.Context, cmd Command, out *Output) (Process, error)

// Process is a running command.
type Process interface {
	// Wait blocks until the command finishes.
	Wait() (exitCode int, signal string, err error)
	// Kill terminates it; the terminal stays valid, as the spec requires.
	Kill() error
}

// Output is a capped, concurrent-safe buffer of a command's output.
type Output struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

// NewOutput makes a buffer with a byte cap; zero means the default.
func NewOutput(limit int) *Output {
	if limit <= 0 {
		limit = OutputLimit
	}
	return &Output{limit: limit}
}

// Write appends output, dropping from the front when it would exceed
// the cap — and remembering that it did, because the protocol has a
// field for exactly that and an agent reading a silently truncated log
// draws confident wrong conclusions from it.
func (o *Output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf = append(o.buf, p...)
	if len(o.buf) > o.limit {
		o.buf = o.buf[len(o.buf)-o.limit:]
		o.truncated = true
	}
	return len(p), nil
}

// Snapshot returns what has been captured so far.
func (o *Output) Snapshot() (text string, truncated bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.buf), o.truncated
}

// terminal is one live terminal's state.
type terminal struct {
	out     *Output
	proc    Process
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	exited  bool
	code    int
	signal  string
	waitErr error
}

// Terminals implements ACP's terminal/* methods over a Runner.
type Terminals struct {
	run Runner

	mu    sync.Mutex
	byID  map[string]*terminal
	nextN atomic.Int64
}

// NewTerminals builds the service.
func NewTerminals(run Runner) *Terminals {
	return &Terminals{run: run, byID: map[string]*terminal{}}
}

// Handle answers one terminal/* request. Anything else is declined, so
// a client built from this refuses methods it did not advertise rather
// than half-answering them.
func (t *Terminals) Handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "terminal/create":
		return t.create(ctx, params)
	case "terminal/output":
		return t.output(params)
	case "terminal/wait_for_exit":
		return t.waitForExit(params)
	case "terminal/kill":
		return t.kill(params)
	case "terminal/release":
		return t.release(params)
	}
	return nil, &Error{Code: CodeMethodNotFound, Message: "method not supported: " + method}
}

type createParams struct {
	SessionID       string `json:"sessionId"`
	Command         string `json:"command"`
	Args            []string
	Cwd             string `json:"cwd"`
	Env             []envVar
	OutputByteLimit *int `json:"outputByteLimit"`
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (t *Terminals) create(ctx context.Context, raw json.RawMessage) (any, error) {
	var p createParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
	}
	if p.Command == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "terminal/create needs a command"}
	}

	limit := 0
	if p.OutputByteLimit != nil {
		limit = *p.OutputByteLimit
	}
	out := NewOutput(limit)

	env := make([]string, 0, len(p.Env))
	for _, e := range p.Env {
		env = append(env, e.Name+"="+e.Value)
	}

	// The command's context is detached from the request's: `create`
	// returns immediately and the process outlives it, which is the
	// whole shape of the capability — the agent polls and waits
	// separately.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	proc, err := t.run(runCtx, Command{
		Command: p.Command, Args: p.Args, Cwd: p.Cwd, Env: env,
	}, out)
	if err != nil {
		cancel()
		return nil, &Error{Code: CodeInternal, Message: err.Error()}
	}

	term := &terminal{out: out, proc: proc, cancel: cancel, done: make(chan struct{})}
	go func() {
		code, sig, werr := proc.Wait()
		term.mu.Lock()
		term.exited, term.code, term.signal, term.waitErr = true, code, sig, werr
		term.mu.Unlock()
		term.once.Do(func() { close(term.done) })
	}()

	id := "term_" + strconv.FormatInt(t.nextN.Add(1), 10)
	t.mu.Lock()
	t.byID[id] = term
	t.mu.Unlock()
	return map[string]any{"terminalId": id}, nil
}

type terminalRef struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

func (t *Terminals) lookup(raw json.RawMessage) (*terminal, error) {
	var ref terminalRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
	}
	t.mu.Lock()
	term, ok := t.byID[ref.TerminalID]
	t.mu.Unlock()
	if !ok {
		return nil, &Error{Code: CodeInvalidParams, Message: "no such terminal: " + ref.TerminalID}
	}
	return term, nil
}

func (t *Terminals) output(raw json.RawMessage) (any, error) {
	term, err := t.lookup(raw)
	if err != nil {
		return nil, err
	}
	text, truncated := term.out.Snapshot()
	resp := map[string]any{"output": text, "truncated": truncated}
	term.mu.Lock()
	if term.exited {
		resp["exitStatus"] = exitStatus(term.code, term.signal)
	}
	term.mu.Unlock()
	return resp, nil
}

func (t *Terminals) waitForExit(raw json.RawMessage) (any, error) {
	term, err := t.lookup(raw)
	if err != nil {
		return nil, err
	}
	<-term.done
	term.mu.Lock()
	defer term.mu.Unlock()
	if term.waitErr != nil && term.code == 0 {
		// A failure to reap is not an exit code; saying it exited 0
		// would tell the agent the command succeeded.
		return nil, &Error{Code: CodeInternal, Message: term.waitErr.Error()}
	}
	return exitStatus(term.code, term.signal), nil
}

func exitStatus(code int, signal string) map[string]any {
	status := map[string]any{}
	if signal != "" {
		status["signal"] = signal
		status["exitCode"] = nil
		return status
	}
	status["exitCode"] = code
	status["signal"] = nil
	return status
}

func (t *Terminals) kill(raw json.RawMessage) (any, error) {
	term, err := t.lookup(raw)
	if err != nil {
		return nil, err
	}
	// The terminal stays valid after a kill, as the spec requires: the
	// agent still wants the output it produced before it was stopped.
	_ = term.proc.Kill()
	return struct{}{}, nil
}

func (t *Terminals) release(raw json.RawMessage) (any, error) {
	var ref terminalRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
	}
	t.mu.Lock()
	term, ok := t.byID[ref.TerminalID]
	delete(t.byID, ref.TerminalID)
	t.mu.Unlock()
	if ok {
		_ = term.proc.Kill()
		term.cancel()
	}
	return struct{}{}, nil
}

// ReleaseAll frees every terminal — what a session's end must do, since
// an agent that forgot a release should not leave a process behind.
func (t *Terminals) ReleaseAll() {
	t.mu.Lock()
	terms := t.byID
	t.byID = map[string]*terminal{}
	t.mu.Unlock()
	for _, term := range terms {
		_ = term.proc.Kill()
		term.cancel()
	}
}

// ExecRunner is the plain runner: run the command as given.
//
// koi wraps this with its sandbox before handing it over; it is
// exported because a runner is the seam, and the unwrapped one is what
// a test — or a caller that has its own policy — wants.
func ExecRunner(ctx context.Context, cmd Command, out *Output) (Process, error) {
	if cmd.Command == "" {
		return nil, errors.New("acp: empty command")
	}
	c := exec.CommandContext(ctx, cmd.Command, cmd.Args...)
	c.Dir = cmd.Cwd
	if len(cmd.Env) > 0 {
		c.Env = append(os.Environ(), cmd.Env...)
	}
	c.Stdout, c.Stderr = out, out
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: c}, nil
}

type execProcess struct{ cmd *exec.Cmd }

func (p *execProcess) Wait() (int, string, error) {
	err := p.cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 0, status.Signal().String(), nil
		}
		return exitErr.ExitCode(), "", nil
	}
	if err != nil {
		return 0, "", err
	}
	return 0, "", nil
}

func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}
