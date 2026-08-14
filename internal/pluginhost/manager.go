package pluginhost

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"

	"github.com/blairham/gish/pkg/pluginapi"
)

// Deadline defaults, from the latency table in docs/plugins.md. Every
// host→plugin call carries one; a plugin that misses is skipped or served
// stale, never awaited.
const (
	// DescribeTimeout bounds plugin startup + Describe.
	DescribeTimeout = 2 * time.Second
	// DefaultRenderBudget is the prompt segment budget when a segment
	// declares none.
	DefaultRenderBudget = 50 * time.Millisecond
	// DefaultCompleteBudget bounds time to the first completion batch.
	DefaultCompleteBudget = 80 * time.Millisecond
)

// DefaultDir returns the plugin discovery directory: every executable in
// $XDG_DATA_HOME/gish/plugins is a tier-2 plugin.
func DefaultDir() (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "gish", "plugins"), nil
}

// Host manages tier-2 plugins: discovery, lazy resident lifecycle,
// Describe-driven capability wiring, and crash healing with backoff.
// Safe for concurrent use. A dead plugin never takes the shell down —
// it is relaunched on next demand once its backoff expires.
type Host struct {
	dir     string
	logOut  io.Writer
	backoff time.Duration // base; doubles per consecutive failure

	seq atomic.Uint64 // event sequence source (stale-response dropping)

	mu      sync.Mutex
	plugins map[string]*pluginState
}

type pluginState struct {
	name string
	path string

	mu           sync.Mutex
	client       *plugin.Client
	proto        plugin.ClientProtocol
	info         *pluginapi.DescribeResponse
	failures     int
	backoffUntil time.Time
}

// Option configures a Host.
type Option func(*Host)

// WithBackoff overrides the base crash backoff (tests use a tiny one).
func WithBackoff(d time.Duration) Option {
	return func(h *Host) { h.backoff = d }
}

// WithLog routes plugin process logs/stderr to w.
func WithLog(w io.Writer) Option {
	return func(h *Host) { h.logOut = w }
}

// NewHost creates a host over a plugin directory. No plugin is launched
// until a capability is demanded.
func NewHost(dir string, opts ...Option) *Host {
	h := &Host{
		dir:     dir,
		logOut:  io.Discard,
		backoff: 5 * time.Second,
		plugins: map[string]*pluginState{},
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// NextSeq returns a session-monotonic event sequence number; consumers
// stamp requests with it and drop responses older than the latest issued.
func (h *Host) NextSeq() uint64 { return h.seq.Add(1) }

// Discover scans the plugin directory. Missing directory means no
// plugins, not an error. Newly appearing binaries are picked up on each
// call; removed ones are dropped (a running client is killed).
func (h *Host) Discover() error {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	seen := map[string]bool{}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.Mode()&0o111 == 0 {
			continue
		}
		name := e.Name()
		seen[name] = true
		if _, ok := h.plugins[name]; !ok {
			h.plugins[name] = &pluginState{name: name, path: filepath.Join(h.dir, name)}
		}
	}
	for name, ps := range h.plugins {
		if !seen[name] {
			ps.shutdown()
			delete(h.plugins, name)
		}
	}
	return nil
}

// Close kills every running plugin. Called on shell exit.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ps := range h.plugins {
		ps.shutdown()
	}
}

func (ps *pluginState) shutdown() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.client != nil {
		ps.client.Kill()
		ps.client, ps.proto, ps.info = nil, nil, nil
	}
}

// ensure launches the plugin if needed and returns its live protocol
// client and description. It self-heals: an exited process is cleaned up
// and relaunched, subject to exponential backoff.
func (ps *pluginState) ensure(ctx context.Context, h *Host) (plugin.ClientProtocol, *pluginapi.DescribeResponse, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.client != nil && !ps.client.Exited() {
		return ps.proto, ps.info, nil
	}
	if ps.client != nil { // crashed since last use
		ps.client.Kill()
		ps.client, ps.proto, ps.info = nil, nil, nil
		ps.noteFailureLocked(h)
	}
	if until := ps.backoffUntil; time.Now().Before(until) {
		return nil, nil, fmt.Errorf("plugin %s: in backoff until %s", ps.name, until.Format(time.TimeOnly))
	}

	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          PluginMap,
		Cmd:              exec.Command(ps.path),
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "plugin." + ps.name,
			Output: h.logOut,
			Level:  hclog.Info,
		}),
	})
	proto, err := client.Client()
	if err != nil {
		client.Kill()
		ps.noteFailureLocked(h)
		return nil, nil, fmt.Errorf("plugin %s: %w", ps.name, err)
	}

	info, err := describe(ctx, proto)
	if err != nil {
		client.Kill()
		ps.noteFailureLocked(h)
		return nil, nil, fmt.Errorf("plugin %s: describe: %w", ps.name, err)
	}

	ps.client, ps.proto, ps.info = client, proto, info
	ps.failures = 0
	ps.backoffUntil = time.Time{}
	return proto, info, nil
}

func (ps *pluginState) noteFailureLocked(h *Host) {
	ps.failures++
	d := h.backoff << (ps.failures - 1)
	if maxB := h.backoff * 16; d > maxB {
		d = maxB
	}
	ps.backoffUntil = time.Now().Add(d)
}

func describe(ctx context.Context, proto plugin.ClientProtocol) (*pluginapi.DescribeResponse, error) {
	raw, err := proto.Dispense("info")
	if err != nil {
		return nil, err
	}
	infoClient, ok := raw.(pluginapi.PluginInfoClient)
	if !ok {
		return nil, fmt.Errorf("info dispensed %T", raw)
	}
	dctx, cancel := context.WithTimeout(ctx, DescribeTimeout)
	defer cancel()
	return infoClient.Describe(dctx, &pluginapi.DescribeRequest{})
}

func hasCap(info *pluginapi.DescribeResponse, c pluginapi.Capability) bool {
	return slices.Contains(info.GetCapabilities(), c)
}

// Provider pairs a live capability client with the plugin it came from.
type Provider[T any] struct {
	Plugin string
	Client T
}

// providers launches (as needed) every discovered plugin advertising c
// and dispenses the named service. Failures are skipped — a broken
// plugin costs its own features, never the shell.
func providers[T any](ctx context.Context, h *Host, c pluginapi.Capability, service string) []Provider[T] {
	h.mu.Lock()
	states := make([]*pluginState, 0, len(h.plugins))
	for _, ps := range h.plugins {
		states = append(states, ps)
	}
	h.mu.Unlock()

	var out []Provider[T]
	for _, ps := range states {
		proto, info, err := ps.ensure(ctx, h)
		if err != nil || !hasCap(info, c) {
			continue
		}
		raw, err := proto.Dispense(service)
		if err != nil {
			continue
		}
		if client, ok := raw.(T); ok {
			out = append(out, Provider[T]{Plugin: ps.name, Client: client})
		}
	}
	slices.SortFunc(out, func(a, b Provider[T]) int {
		return strings.Compare(a.Plugin, b.Plugin)
	})
	return out
}

// PromptProviders returns live prompt-segment clients.
func (h *Host) PromptProviders(ctx context.Context) []Provider[pluginapi.PromptSegmentProviderClient] {
	return providers[pluginapi.PromptSegmentProviderClient](ctx, h, pluginapi.Capability_CAPABILITY_PROMPT_SEGMENT, "prompt")
}

// CompletionProviders returns live completion clients.
func (h *Host) CompletionProviders(ctx context.Context) []Provider[pluginapi.CompletionProviderClient] {
	return providers[pluginapi.CompletionProviderClient](ctx, h, pluginapi.Capability_CAPABILITY_COMPLETION, "completion")
}

// HistoryBackends returns live history backend clients.
func (h *Host) HistoryBackends(ctx context.Context) []Provider[pluginapi.HistoryBackendClient] {
	return providers[pluginapi.HistoryBackendClient](ctx, h, pluginapi.Capability_CAPABILITY_HISTORY, "history")
}

// Status describes one discovered plugin for the `plugins` builtin.
type Status struct {
	Name         string
	Running      bool
	Capabilities []string
	Version      string
	BackoffUntil time.Time
}

// Statuses launches nothing; it reports what discovery and prior use
// know. Pass describe=true to launch and interrogate every plugin.
func (h *Host) Statuses(ctx context.Context, describeAll bool) []Status {
	h.mu.Lock()
	states := make([]*pluginState, 0, len(h.plugins))
	for _, ps := range h.plugins {
		states = append(states, ps)
	}
	h.mu.Unlock()

	out := make([]Status, 0, len(states))
	for _, ps := range states {
		if describeAll {
			_, _, _ = ps.ensure(ctx, h) //nolint:errcheck // reflected in status below
		}
		ps.mu.Lock()
		st := Status{
			Name:         ps.name,
			Running:      ps.client != nil && !ps.client.Exited(),
			BackoffUntil: ps.backoffUntil,
		}
		if ps.info != nil {
			st.Version = ps.info.GetVersion()
			for _, c := range ps.info.GetCapabilities() {
				st.Capabilities = append(st.Capabilities, capName(c))
			}
		}
		ps.mu.Unlock()
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b Status) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func capName(c pluginapi.Capability) string {
	switch c {
	case pluginapi.Capability_CAPABILITY_COMPLETION:
		return "completion"
	case pluginapi.Capability_CAPABILITY_PROMPT_SEGMENT:
		return "prompt"
	case pluginapi.Capability_CAPABILITY_HISTORY:
		return "history"
	default:
		return "unknown"
	}
}
