package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/pkg/pluginapi"
)

// CommandIndex routes command names to the plugins that provide them
// (#11). Names come from an mtime-keyed cache so a warm session knows
// its plugin commands without launching anything; new or changed plugin
// binaries are re-interrogated asynchronously at startup, and the owning
// plugin launches lazily on first invocation.
//
// Precedence (decided on #11): reserved names (interpreter and
// gish-native builtins) are rejected with a warning; shell functions
// shadow these automatically (dispatched before the exec seam); plugin
// commands shadow PATH. Contested names go to the lexicographically
// first plugin.
type CommandIndex struct {
	host     *Host
	reserved func(string) bool
	cachedAt string

	// persisting tracks in-flight cache writes. The interrogation runs
	// detached so discovery never blocks a prompt, but that goroutine
	// creates and writes $XDG_STATE_HOME/gish — so something has to be
	// able to wait for it. Without this, a shell (or a test) that exits
	// while it is running races the write against its own teardown; on
	// Windows that surfaces as "directory is not empty" when the state
	// dir is removed out from under it.
	persisting sync.WaitGroup

	mu     sync.Mutex
	byName map[string]string          // command → plugin
	specs  map[string][]cachedCommand // plugin → its commands
}

// Wait blocks until any in-flight cache persistence has finished.
// Callers that are about to remove or inspect the cache directory —
// shutdown paths and tests — need it; the interactive loop does not.
func (ci *CommandIndex) Wait() { ci.persisting.Wait() }

type indexFile struct {
	Plugins map[string]indexEntry `json:"plugins"`
}

type indexEntry struct {
	Path      string          `json:"path"`
	MtimeUnix int64           `json:"mtime_unix"`
	Commands  []cachedCommand `json:"commands"`
}

type cachedCommand struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
}

// defaultIndexPath is the cache location: $XDG_STATE_HOME/gish/
// command-index.json (state, not config: derived and disposable).
func defaultIndexPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "gish", "command-index.json")
}

// NewCommandIndex builds the index from the cache and kicks off an
// asynchronous refresh for plugins whose binaries are new or changed.
// reserved reports names plugins may not claim.
func (h *Host) NewCommandIndex(reserved func(string) bool) *CommandIndex {
	ci := &CommandIndex{
		host:     h,
		reserved: reserved,
		cachedAt: defaultIndexPath(),
		byName:   map[string]string{},
		specs:    map[string][]cachedCommand{},
	}
	cache := ci.loadCache()

	h.mu.Lock()
	states := make([]*pluginState, 0, len(h.plugins))
	for _, ps := range h.plugins {
		states = append(states, ps)
	}
	h.mu.Unlock()

	var stale []*pluginState
	fresh := indexFile{Plugins: map[string]indexEntry{}}
	for _, ps := range states {
		entry, ok := cache.Plugins[ps.name]
		mtime := binaryMtime(ps.path)
		if ok && entry.Path == ps.path && entry.MtimeUnix == mtime && mtime != 0 {
			fresh.Plugins[ps.name] = entry
			ci.setCommands(ps.name, entry.Commands)
			continue
		}
		stale = append(stale, ps)
	}

	if len(stale) > 0 {
		ci.persisting.Add(1)
		go func() {
			defer ci.persisting.Done()
			for _, ps := range stale {
				commands := ci.interrogate(ps)
				ci.setCommands(ps.name, commands)
				fresh.Plugins[ps.name] = indexEntry{
					Path:      ps.path,
					MtimeUnix: binaryMtime(ps.path),
					Commands:  commands,
				}
			}
			ci.saveCache(fresh)
		}()
	} else {
		ci.saveCache(fresh) // prunes removed plugins
	}
	return ci
}

func binaryMtime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

func (ci *CommandIndex) loadCache() indexFile {
	cache := indexFile{Plugins: map[string]indexEntry{}}
	if ci.cachedAt == "" {
		return cache
	}
	data, err := os.ReadFile(ci.cachedAt)
	if err != nil {
		return cache
	}
	_ = json.Unmarshal(data, &cache) //nolint:errcheck // corrupt cache = cold start
	if cache.Plugins == nil {
		cache.Plugins = map[string]indexEntry{}
	}
	return cache
}

func (ci *CommandIndex) saveCache(f indexFile) {
	if ci.cachedAt == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(ci.cachedAt), 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	// Write-then-rename: the file's existence must mean its content is
	// complete — a warm-starting index (or a test polling for the file)
	// must never read a half-written cache.
	tmp := ci.cachedAt + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, ci.cachedAt) //nolint:errcheck // cache is disposable
}

// interrogate launches a plugin and asks for its commands.
func (ci *CommandIndex) interrogate(ps *pluginState) []cachedCommand {
	ctx, cancel := context.WithTimeout(context.Background(), DescribeTimeout)
	defer cancel()
	proto, info, err := ps.ensure(ctx, ci.host)
	if err != nil || !hasCap(info, pluginapi.Capability_CAPABILITY_COMMAND) {
		return nil
	}
	raw, err := proto.Dispense("command")
	if err != nil {
		return nil
	}
	client, ok := raw.(pluginapi.CommandProviderClient)
	if !ok {
		return nil
	}
	resp, err := client.Commands(ctx, &pluginapi.CommandsRequest{})
	if err != nil {
		return nil
	}
	out := make([]cachedCommand, 0, len(resp.GetCommands()))
	for _, c := range resp.GetCommands() {
		out = append(out, cachedCommand{Name: c.GetName(), Summary: c.GetSummary()})
	}
	return out
}

// setCommands records a plugin's commands, applying the reserved-name
// and first-plugin-wins collision rules.
func (ci *CommandIndex) setCommands(pluginName string, commands []cachedCommand) {
	ci.mu.Lock()
	defer ci.mu.Unlock()
	// Drop this plugin's previous claims, then re-resolve every name so
	// collision winners stay deterministic (lexicographic plugin order).
	for name, owner := range ci.byName {
		if owner == pluginName {
			delete(ci.byName, name)
		}
	}
	ci.specs[pluginName] = commands
	plugins := make([]string, 0, len(ci.specs))
	for name := range ci.specs {
		plugins = append(plugins, name)
	}
	slices.Sort(plugins)
	for _, plug := range plugins {
		for _, c := range ci.specs[plug] {
			if ci.reserved != nil && ci.reserved(c.Name) {
				fmt.Fprintf(os.Stderr, "gish: plugin %s: command %q is a reserved builtin name, ignored\n", plug, c.Name)
				continue
			}
			if owner, taken := ci.byName[c.Name]; taken && owner != plug {
				fmt.Fprintf(os.Stderr, "gish: plugin %s: command %q already provided by %s, ignored\n", plug, c.Name, owner)
				continue
			}
			ci.byName[c.Name] = plug
		}
	}
}

// CommandsOf lists a plugin's registered commands, for `plugins`.
func (ci *CommandIndex) CommandsOf(pluginName string) []string {
	ci.mu.Lock()
	defer ci.mu.Unlock()
	var out []string
	for name, owner := range ci.byName {
		if owner == pluginName {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func (ci *CommandIndex) lookup(name string) string {
	ci.mu.Lock()
	defer ci.mu.Unlock()
	return ci.byName[name]
}

// ExecMiddleware dispatches plugin-provided commands: after gish-native
// builtins, before PATH execution.
func (ci *CommandIndex) ExecMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		plug := ci.lookup(args[0])
		if plug == "" {
			return next(ctx, args)
		}
		return ci.run(ctx, plug, args)
	}
}

// envAllowlist is the environment a plugin command receives — the
// filtered-env invariant applied to command execution.
var envAllowlist = []string{"PATH", "HOME", "TERM", "LANG", "LC_ALL", "USER"}

func allowedEnv(env expand.Environ) map[string]string {
	out := make(map[string]string, len(envAllowlist))
	for _, name := range envAllowlist {
		if v := env.Get(name); v.IsSet() {
			out[name] = v.String()
		}
	}
	return out
}

// run executes one plugin command invocation, streaming I/O between the
// interpreter's handler context and the plugin.
func (ci *CommandIndex) run(ctx context.Context, pluginName string, args []string) error {
	hc := interp.HandlerCtx(ctx)
	fail := func(err error) error {
		fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], err)
		return interp.ExitStatus(126)
	}

	ci.host.mu.Lock()
	ps := ci.host.plugins[pluginName]
	ci.host.mu.Unlock()
	if ps == nil {
		return fail(errors.New("plugin has disappeared"))
	}
	proto, _, err := ps.ensure(ctx, ci.host)
	if err != nil {
		return fail(err)
	}
	raw, err := proto.Dispense("command")
	if err != nil {
		return fail(err)
	}
	client, ok := raw.(pluginapi.CommandProviderClient)
	if !ok {
		return fail(fmt.Errorf("command dispensed %T", raw))
	}

	stream, err := client.Run(ctx)
	if err != nil {
		return fail(err)
	}
	if err := stream.Send(&pluginapi.RunInput{Input: &pluginapi.RunInput_Start{Start: &pluginapi.RunStart{
		Name: args[0],
		Args: args[1:],
		Cwd:  hc.Dir,
		Env:  allowedEnv(hc.Env),
	}}}); err != nil {
		return fail(err)
	}

	// Pump stdin as the command consumes it; the goroutine winds down
	// with the stream (Send fails after close) or on EOF.
	go pumpStdin(stream, hc.Stdin)

	for {
		out, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil // stream ended without exit: treat as success
			}
			if ctx.Err() != nil {
				return ctx.Err() // interrupted (#3): cancellation, not failure
			}
			return fail(err)
		}
		switch msg := out.GetOutput().(type) {
		case *pluginapi.RunOutput_Stdout:
			_, _ = hc.Stdout.Write(msg.Stdout) //nolint:errcheck // interpreter-owned writer
		case *pluginapi.RunOutput_Stderr:
			_, _ = hc.Stderr.Write(msg.Stderr) //nolint:errcheck // interpreter-owned writer
		case *pluginapi.RunOutput_Exit:
			if msg.Exit == 0 {
				return nil
			}
			return interp.ExitStatus(msg.Exit) //nolint:gosec // proto int32 exit status
		}
	}
}

func pumpStdin(stream pluginapi.CommandProvider_RunClient, stdin io.Reader) {
	defer func() {
		_ = stream.CloseSend() //nolint:errcheck // stream teardown
	}()
	if stdin == nil {
		_ = stream.Send(&pluginapi.RunInput{Input: &pluginapi.RunInput_StdinEof{StdinEof: true}}) //nolint:errcheck
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pluginapi.RunInput{Input: &pluginapi.RunInput_Stdin{Stdin: slices.Clone(buf[:n])}}); serr != nil {
				return
			}
		}
		if err != nil {
			_ = stream.Send(&pluginapi.RunInput{Input: &pluginapi.RunInput_StdinEof{StdinEof: true}}) //nolint:errcheck
			return
		}
	}
}
