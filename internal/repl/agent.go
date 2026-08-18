package repl

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/history"
	"github.com/blairham/koi-shell/internal/pluginhost"
	pluginapi "github.com/blairham/koi-shell/pkg/pluginapi/v1"
)

// EXPERIMENTAL, FROZEN (#111). koi's position is that it hosts other
// people's agents rather than being one: the researched demand for AI in
// a shell is `??` and `explain`, and sandbox profiles plus gated env are
// what a shell uniquely contributes to agents that already run inside
// it. This surface stays because execution belongs to the shell — a
// plugin may never hold an exec channel, so orchestration cannot move
// behind the plugin boundary without weakening that — but it is
// unadvertised and receives no further investment. Do not extend it
// without new demand data.
//
// The agent surface (#34), Kiro-style: give it a task, it plans first —
// the spec renders in the terminal and saves as an artifact — and
// nothing executes until approved. The intelligence is any AIProvider
// (the Plan RPC); the orchestration is deliberately shell-native,
// because only the shell can run steps in the real session, and a
// plugin must never hold an exec channel.
//
// Gates: [a]ll runs the plan with destructive steps still gating
// individually; [s]tep gates every step. At a gate, escalation out of
// the sandbox is its own explicit answer — never a default. A failing
// step halts the plan.

// agentDeps carries the seams the loop injects: approval I/O and the
// real execution path (BeginLine → runInterruptible → EndLine).
type agentDeps struct {
	runner *interp.Runner
	in     io.Reader
	out    io.Writer
	exec   func(line string) int
	// choose is the interactive frontend (huh select); nil falls back
	// to single-rune line input — same keys either way.
	choose chooser
}

// decide asks one gate question through whichever frontend exists.
func (d agentDeps) decide(scanner *bufio.Scanner, prompt string, options []chooseOption) (string, bool) {
	if d.choose != nil {
		return d.choose(prompt, options)
	}
	keys := make([]string, len(options))
	hints := make([]string, len(options))
	for i, o := range options {
		keys[i] = o.key
		hints[i] = o.key + "=" + o.label
	}
	return ask(d.out, scanner, prompt+" ("+strings.Join(hints, " / ")+")", strings.Join(keys, ""))
}

// plan asks the provider for a step spec.
func (m *aiManager) plan(ctx context.Context, runner *interp.Runner, task string) (*pluginapi.PlanResponse, error) {
	client, _, err := m.provider(ctx, runner)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, pluginhost.AITimeout)
	defer cancel()
	resp, err := client.Plan(pctx, &pluginapi.PlanRequest{
		Task:     task,
		Context:  m.shellContext(runner),
		EventSeq: m.nextSeq(),
	})
	if err != nil {
		return nil, err
	}
	if len(resp.GetSteps()) == 0 {
		return nil, fmt.Errorf("provider returned an empty plan")
	}
	return resp, nil
}

// stepDestructive is the shell's own judgment, OR'd with the provider's
// flag: every command name in the step is checked against the lint
// destructive set, and a parse failure counts as destructive — what
// cannot be judged must gate.
func stepDestructive(command string) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "agent-step")
	if err != nil {
		return true
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok && len(call.Args) > 0 {
			if lit, ok := call.Args[0].Parts[0].(*syntax.Lit); ok && destructive[lit.Value] {
				found = true
			}
		}
		return !found
	})
	return found
}

// handleAgent drives one task end to end.
func handleAgent(ctx context.Context, deps agentDeps, task string) {
	if aiMgr == nil {
		fmt.Fprintln(deps.out, "koi: agent: no plugin host in this session")
		return
	}
	task = unquoteTask(task)
	if task == "" {
		fmt.Fprintln(deps.out, `usage: agent <task>   e.g. agent "move every .log file into logs/"`)
		return
	}

	plan, err := aiMgr.plan(ctx, deps.runner, task)
	if err != nil {
		fmt.Fprintln(deps.out, "koi: agent:", err)
		return
	}

	// Mark destructiveness once: provider's flag OR the shell's parse.
	gated := make([]bool, len(plan.GetSteps()))
	for i, step := range plan.GetSteps() {
		gated[i] = step.GetDestructive() || stepDestructive(step.GetCommand())
	}

	fmt.Fprintf(deps.out, "\nplan: %s\n", plan.GetSummary())
	for i, step := range plan.GetSteps() {
		marker := " "
		if gated[i] {
			marker = "⚠"
		}
		fmt.Fprintf(deps.out, "  %d.%s %s\n     $ %s\n", i+1, marker, step.GetTitle(), step.GetCommand())
	}
	artifact := savePlanArtifact(task, plan)
	if artifact != "" {
		fmt.Fprintf(deps.out, "plan saved: %s\n", displayPath(artifact))
	}

	scanner := bufio.NewScanner(deps.in)
	mode, ok := deps.decide(scanner, "run this plan?", []chooseOption{
		{"a", "run all — destructive steps still gate individually"},
		{"s", "step-by-step"},
		{"q", "quit — nothing executes"},
	})
	if !ok || mode == "q" {
		fmt.Fprintln(deps.out, "agent: nothing executed")
		return
	}

	for i, step := range plan.GetSteps() {
		fmt.Fprintf(deps.out, "\nstep %d/%d: %s\n  $ %s\n", i+1, len(plan.GetSteps()), step.GetTitle(), step.GetCommand())
		line := sandboxWrap(deps.runner, step.GetCommand(), "KOI_AGENT_SANDBOX")
		if mode == "s" || gated[i] {
			warn := ""
			if gated[i] {
				warn = " (destructive)"
			}
			answer, aok := deps.decide(scanner, fmt.Sprintf("run step %d%s?", i+1, warn), []chooseOption{
				{"r", "run (sandboxed)"},
				{"!", "run WITHOUT the sandbox — escalate"},
				{"k", "skip this step"},
				{"q", "quit the plan"},
			})
			switch {
			case !aok || answer == "q":
				appendOutcome(artifact, i, "halted by user")
				fmt.Fprintln(deps.out, "agent: plan halted")
				return
			case answer == "k":
				appendOutcome(artifact, i, "skipped")
				continue
			case answer == "!":
				// Escalation out of the sandbox is its own approval (#21).
				line = step.GetCommand()
			}
		}
		exit := deps.exec(line)
		if exit != 0 {
			appendOutcome(artifact, i, fmt.Sprintf("failed (exit %d)", exit))
			fmt.Fprintf(deps.out, "agent: step %d failed (exit %d) — plan halted\n", i+1, exit)
			return
		}
		appendOutcome(artifact, i, "ok")
	}
	fmt.Fprintf(deps.out, "agent: plan complete (%d step(s))\n", len(plan.GetSteps()))
}

// ask prompts until one of the allowed single-rune answers arrives
// (Enter picks the first). ok=false on EOF.
func ask(out io.Writer, scanner *bufio.Scanner, prompt, allowed string) (string, bool) {
	for {
		fmt.Fprintf(out, "%s: ", prompt)
		if !scanner.Scan() {
			fmt.Fprintln(out)
			return "", false
		}
		answer := strings.TrimSpace(scanner.Text())
		if answer == "" {
			return string(allowed[0]), true
		}
		if len(answer) == 1 && strings.Contains(allowed, answer) {
			return answer, true
		}
		fmt.Fprintf(out, "  answer one of: %s\n", strings.Join(strings.Split(allowed, ""), " / "))
	}
}

// unquoteTask strips one layer of shell quotes: the line is intercepted
// before parsing, so `agent "do x"` arrives quotes and all.
func unquoteTask(task string) string {
	task = strings.TrimSpace(task)
	if len(task) >= 2 && (task[0] == '"' || task[0] == '\'') && task[len(task)-1] == task[0] {
		task = task[1 : len(task)-1]
	}
	return strings.TrimSpace(task)
}

// savePlanArtifact writes the spec under the session's data dir — plans
// are artifacts, not scrollback. Best-effort: a write failure costs the
// artifact, never the plan.
func savePlanArtifact(task string, plan *pluginapi.PlanResponse) string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(dataHome, "koi", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	path := filepath.Join(dir, time.Now().Format("20060102-150405")+".md")
	var b strings.Builder
	fmt.Fprintf(&b, "# agent plan — %s\n\ntask: %s\n\n%s\n\n",
		time.Now().Format(time.RFC3339), redactForArtifact(task), plan.GetSummary())
	for i, step := range plan.GetSteps() {
		fmt.Fprintf(&b, "%d. %s\n   `%s`\n", i+1, step.GetTitle(), redactForArtifact(step.GetCommand()))
	}
	b.WriteString("\n## outcomes\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return ""
	}
	return path
}

// redactForArtifact applies the #10 rules to text on its way into a
// plan artifact.
//
// Both halves need it. The task is what the user typed and can contain
// anything they typed; a step's command comes from a model, which was
// given scrub-safe context but can still echo back a literal it was
// shown or invent one. An artifact outlives the session, so this is the
// same class of exposure as history and sessions.
//
// Redaction rather than refusal, unlike a history entry: an artifact
// with one line masked is still a usable record of what was planned,
// whereas refusing to write it loses the plan entirely — and the plan is
// the only durable trace of what the shell was asked to do.
func redactForArtifact(s string) string {
	clean, _ := history.RedactOutput([]byte(s))
	return string(clean)
}

// appendOutcome records what happened to one step in the artifact.
func appendOutcome(artifact string, step int, outcome string) {
	if artifact == "" {
		return
	}
	f, err := os.OpenFile(artifact, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // our own artifact
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "- step %d: %s\n", step+1, outcome)
}
