package main

import (
	"strings"
	"testing"

	pluginapi "github.com/blairham/gish/pkg/pluginapi/v1"
)

// The parsing an agent's reply goes through, which is the only part of
// this plugin with a decision in it: everything else is protocol.

func TestSplitCandidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, out, command, explanation string
	}{
		{"plain", "ls -la\n", "ls -la", ""},
		{"with rationale", "git status\n# shows what changed\n", "git status", "shows what changed"},
		{"leading blank lines", "\n\nmake test\n", "make test", ""},
		// Agents fence code by habit, and a fenced command that arrives
		// as `` `ls` `` would be run with the backticks — which in a
		// shell is command substitution, not a quoting mistake.
		{"fenced", "`du -sh *`\n", "du -sh *", ""},
		{"nothing", "\n\n", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, explanation := splitCandidate(tt.out)
			if command != tt.command || explanation != tt.explanation {
				t.Errorf("got %q/%q, want %q/%q", command, explanation, tt.command, tt.explanation)
			}
		})
	}
}

// The prompt must tell the agent not to act. This provider declines the
// terminal capability outright, so an agent that tried would be refused
// — but saying so avoids a turn spent finding that out.
func TestPromptsForbidExecution(t *testing.T) {
	t.Parallel()

	compose := composePrompt(&pluginapi.ComposeRequest{Query: "list files"})
	if !strings.Contains(compose, "Do not run anything") {
		t.Errorf("compose prompt does not forbid execution:\n%s", compose)
	}
	explain := explainPrompt(&pluginapi.ExplainRequest{Command: "make", ExitCode: 2})
	if !strings.Contains(explain, "Do not run anything") {
		t.Errorf("explain prompt does not forbid execution:\n%s", explain)
	}
}

// The agent is configurable, because "any ACP agent" is the point.
func TestAgentArgv(t *testing.T) {
	t.Setenv("GISH_ACP_AGENT", "")
	if got := agentArgv(); len(got) != 1 || got[0] != "claude-code-acp" {
		t.Errorf("default agent = %v", got)
	}
	t.Setenv("GISH_ACP_AGENT", "gemini --acp")
	if got := agentArgv(); len(got) != 2 || got[1] != "--acp" {
		t.Errorf("configured agent = %v", got)
	}
}
