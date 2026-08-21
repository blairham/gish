package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/history"
	"github.com/blairham/koi-shell/internal/pluginhost"
	"github.com/blairham/koi-shell/internal/sandbox"
	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
)

// AI-native integration (#20): the shell owns the hooks, an AIProvider
// plugin owns the model. Correctness rules enforced here:
//
//   - Explicitly invoked only — the ?? prefix and the explain builtin
//     are the entire surface; nothing ambient.
//   - Preview-before-execute — compose results land in the editor
//     buffer, wrapped by default in a visible #21 sandbox invocation
//     the user can edit away. AI output NEVER auto-executes.
//   - Scrubbed context — recent commands come from the history store,
//     which never records secret-bearing commands (#10); env is the
//     allowlist. Nothing else leaves the machine.

// aiMgr is set at interactive startup when a plugin host exists.
var aiMgr *aiManager

type aiManager struct {
	nextSeq func() uint64
	store   *history.Store // nil = history disabled

	warmed    chan struct{}
	providers []pluginhost.Provider[pluginapi.AIProviderClient]

	lastExit int
}

func newAIManager(host *pluginhost.Host, store *history.Store) *aiManager {
	m := &aiManager{
		nextSeq: host.NextSeq,
		store:   store,
		warmed:  make(chan struct{}),
	}
	go func() {
		defer close(m.warmed)
		ctx, cancel := context.WithTimeout(context.Background(), pluginhost.DescribeTimeout)
		defer cancel()
		m.providers = host.AIProviders(ctx)
	}()
	return m
}

// note records the last command's exit code for context assembly.
func (m *aiManager) note(exit int) { m.lastExit = exit }

// provider picks the model plugin: KOI_AI_PROVIDER selects by plugin
// name, otherwise the first discovered provider wins.
func (m *aiManager) provider(ctx context.Context, runner *interp.Runner) (pluginapi.AIProviderClient, string, error) {
	select {
	case <-m.warmed:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	if len(m.providers) == 0 {
		return nil, "", errors.New("no AI provider plugin installed (see docs/plugins.md)")
	}
	if want := shellVar(runner, "KOI_AI_PROVIDER", ""); want != "" {
		for _, p := range m.providers {
			if p.Plugin == want {
				return p.Client, p.Plugin, nil
			}
		}
		return nil, "", fmt.Errorf("KOI_AI_PROVIDER=%q matches no installed plugin", want)
	}
	return m.providers[0].Client, m.providers[0].Plugin, nil
}

// shellContext assembles what the provider may see: cwd, exit code,
// allowlisted env, OS, and scrub-safe recent history.
func (m *aiManager) shellContext(runner *interp.Runner) *pluginapi.ShellContext {
	sc := &pluginapi.ShellContext{
		Cwd:          runner.Dir,
		LastExitCode: int32(m.lastExit),
		Env:          allowedShellEnv(runner),
		Os:           runtime.GOOS,
	}
	if m.store != nil {
		sc.RecentCommands = m.store.Recent(10)
	}
	return sc
}

// compose asks the provider for a command; the first streamed candidate
// wins (the contract is best-first).
func (m *aiManager) compose(ctx context.Context, runner *interp.Runner, query string) (command, explanation string, err error) {
	client, _, err := m.provider(ctx, runner)
	if err != nil {
		return "", "", err
	}
	cctx, cancel := context.WithTimeout(ctx, pluginhost.AITimeout)
	defer cancel()
	stream, err := client.Compose(cctx, &pluginapi.ComposeRequest{
		Query:    query,
		Context:  m.shellContext(runner),
		EventSeq: m.nextSeq(),
	})
	if err != nil {
		return "", "", err
	}
	for {
		cand, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return "", "", errors.New("provider returned no candidates")
			}
			return "", "", rerr
		}
		if cmd := strings.TrimSpace(cand.GetCommand()); cmd != "" {
			return cmd, strings.TrimSpace(cand.GetExplanation()), nil
		}
	}
}

// explain asks "why did that fail" for the most recent command — taken
// from the history store, so a secret-bearing command (never recorded)
// can never be sent.
func (m *aiManager) explain(ctx context.Context, runner *interp.Runner) (string, error) {
	if m.store == nil {
		return "", errors.New("history is disabled — nothing safe to explain")
	}
	recent := m.store.Recent(1)
	if len(recent) == 0 {
		return "", errors.New("no command to explain yet")
	}
	client, _, err := m.provider(ctx, runner)
	if err != nil {
		return "", err
	}
	ectx, cancel := context.WithTimeout(ctx, pluginhost.AITimeout)
	defer cancel()
	resp, err := client.Explain(ectx, &pluginapi.ExplainRequest{
		Command:  recent[0],
		ExitCode: int32(m.lastExit),
		Context:  m.shellContext(runner),
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.GetExplanation()), nil
}

// noSandboxWrap lists command heads that must run in the shell process
// — wrapping them in a sandboxed child would change their meaning.
var noSandboxWrap = []string{"cd", "export", "unset", "alias", "unalias", "source", ".", "eval", "exit"}

// sandboxWrap wraps an AI-proposed command in a visible sandbox
// invocation (#21's safe-by-construction pairing) the user can see and
// remove. profileVar names the knob (KOI_AI_SANDBOX for compose,
// KOI_AGENT_SANDBOX for agent steps); "off" disables. Shell-state
// commands and already-sandboxed sessions pass through unwrapped.
func sandboxWrap(runner *interp.Runner, command, profileVar string) string {
	profile := shellVar(runner, profileVar, "workspace")
	if profile == "off" || sessionSandboxProfile != "" || strings.Contains(command, "\n") {
		return command
	}
	if _, err := sandbox.Resolve(profile, ""); err != nil {
		return command
	}
	if fields := strings.Fields(command); len(fields) > 0 && slices.Contains(noSandboxWrap, fields[0]) {
		return command
	}
	return "sandbox --profile " + profile + " -- " + command
}

// composePrefill is the ?? flavor of sandboxWrap.
func composePrefill(runner *interp.Runner, command string) string {
	return sandboxWrap(runner, command, "KOI_AI_SANDBOX")
}

// handleCompose is the ?? path: query the provider, preload the editor
// buffer with the (sandbox-wrapped) candidate for review.
func handleCompose(ctx context.Context, runner *interp.Runner, query string, preload func(string), out io.Writer) {
	if aiMgr == nil {
		fmt.Fprintln(out, "koi: ai: no plugin host in this session")
		return
	}
	if query == "" {
		fmt.Fprintln(out, "usage: ?? <what you want done>")
		return
	}
	command, explanation, err := aiMgr.compose(ctx, runner, query)
	if err != nil {
		fmt.Fprintln(out, "koi: ai:", err)
		return
	}
	if explanation != "" {
		fmt.Fprintln(out, cDim+"# "+explanation+cReset)
	}
	preload(composePrefill(runner, command))
}

// explainCallHandler intercepts `explain`, config-style.
func explainCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "explain" {
			return next(ctx, args)
		}
		hc := interp.HandlerCtx(ctx)
		if aiMgr == nil || sessionRunner() == nil {
			hc.Errf("explain: no AI provider in this session\n")
			return []string{"false"}, nil
		}
		explanation, err := aiMgr.explain(ctx, sessionRunner())
		if err != nil {
			hc.Errf("explain: %v\n", err)
			return []string{"false"}, nil
		}
		fmt.Fprintln(hc.Stdout, explanation)
		return []string{"true"}, nil
	}
}
