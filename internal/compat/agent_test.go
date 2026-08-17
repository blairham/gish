//go:build unix

package compat_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/blairham/gish/internal/compat"
)

// The agent gate (#208). The claim being defended is specific: an agent
// pointed at gish gets the user's real environment — functions, aliases,
// PATH — with no syntax-error preamble, through the invocation shapes
// harnesses actually write.
//
// This is a gate, not a report: a regression here is the difference
// between "my coding agent works" and the dual-shell split gish exists
// to collapse, and it fails silently in the wild, which is why it has to
// fail loudly here.
func TestAgentGate(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH; the oracle is required")
	}
	gishBin := buildGish(t)

	results := compat.RunAgentAll(context.Background(), bashBin, gishBin)
	if len(results) != len(compat.AgentCorpus) {
		t.Fatalf("ran %d cases, corpus has %d", len(results), len(compat.AgentCorpus))
	}
	for _, r := range results {
		if r.Pass {
			continue
		}
		t.Errorf("%s: %s\n  provenance: %s\n  argv: %q\n  bash(%d): %q\n  gish(%d): %q",
			r.Name, r.Reason, r.Provenance, r.Argv, r.BashCode, r.BashOut, r.GishCode, r.GishOut)
	}
}

// The two deliberate divergences are the ones most likely to be "fixed"
// by someone making the gate green, so they assert the difference rather
// than tolerating it.
func TestAgentGateKeepsShellIdentityDistinct(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	gishBin := buildGish(t)

	var found bool
	for _, c := range compat.AgentCorpus {
		if c.Expect == "" {
			continue
		}
		found = true
		if c.Why == "" {
			t.Errorf("%s: a deliberate divergence with no stated reason is indistinguishable from a bug", c.Name)
		}
		r := compat.RunAgent(context.Background(), bashBin, gishBin, c)
		if !r.Pass {
			t.Errorf("%s: gish printed %q, want %q", c.Name, r.GishOut, c.Expect)
		}
	}
	if !found {
		t.Error("no divergence cases in the corpus; $0 and BASH_VERSION are decided (#120) and should be pinned")
	}
}

// Every case must say where it came from. The gate's value is that a
// reader can re-verify the claim against the harness rather than trust
// this file, and a case with no provenance cannot be re-verified.
func TestAgentCorpusIsAttributed(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range compat.AgentCorpus {
		if strings.TrimSpace(c.Provenance) == "" {
			t.Errorf("%s: no provenance", c.Name)
		}
		if len(c.Argv) == 0 {
			t.Errorf("%s: no argv", c.Name)
		}
		if seen[c.Name] {
			t.Errorf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
	}
}

// A harness that hardcodes `bash -c` still has to work — that is the
// point of the compatibility claim, and it is the case people assume is
// broken.
func TestAgentHardcodedBashStillWorks(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	gishBin := buildGish(t)

	// An agent that ignores $SHELL entirely gets real bash, and gish
	// running the same thing must agree with it. If this ever diverges,
	// "it still works, that's the point" stops being true.
	c := compat.AgentCase{
		Name:       "hardcoded bash -c",
		Provenance: "anthropics/claude-code#11475 — agents ignore the user's default shell",
		Argv:       []string{"-c", `set -e; x=1; for i in 1 2 3; do x=$((x*2)); done; echo "$x"`},
	}
	if r := compat.RunAgent(context.Background(), bashBin, gishBin, c); !r.Pass {
		t.Errorf("%s: %s\n  bash(%d): %q\n  gish(%d): %q",
			c.Name, r.Reason, r.BashCode, r.BashOut, r.GishCode, r.GishOut)
	}
}
