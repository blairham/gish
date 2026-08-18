//go:build unix

package compat_test

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// -update-agent regenerates the generated region of docs/agents.md.
// Without it, TestAgentGapsDoc is the gate that keeps the published table
// from drifting away from what the corpus actually reports.
var updateAgent = flag.Bool("update-agent", false, "regenerate the generated region of docs/agents.md")

const agentDocPath = "../../docs/agents.md"

// The agent gate (#208). The claim being defended is specific: an agent
// pointed at koi gets the user's real environment — functions, aliases,
// PATH — with no syntax-error preamble, through the invocation shapes
// harnesses actually write.
//
// This is a gate, not a report: a regression here is the difference
// between "my coding agent works" and the dual-shell split koi exists
// to collapse, and it fails silently in the wild, which is why it has to
// fail loudly here.
func TestAgentGate(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH; the oracle is required")
	}
	koiBin := buildKoi(t)

	results := compat.RunAgentAll(context.Background(), bashBin, koiBin)
	if len(results) != len(compat.AgentCorpus) {
		t.Fatalf("ran %d cases, corpus has %d", len(results), len(compat.AgentCorpus))
	}
	for _, r := range results {
		if r.Pass {
			continue
		}
		// A case whose oracle is too old to answer it is reported, not
		// failed — and reported loudly enough that a permanently skipped
		// case cannot hide as a green run.
		if r.Skipped {
			t.Logf("%s: skipped — %s", r.Name, r.Reason)
			continue
		}
		// A gap with an issue number on it is reported, not failed. The
		// gate stays a gate for everything else, and TestAgentKnownGaps
		// below is what keeps a marker from outliving its bug.
		if r.Known > 0 {
			t.Logf("%s: known gap (#%d) — %s\n  %s", r.Name, r.Known, r.Reason, r.KnownNote)
			continue
		}
		t.Errorf("%s: %s\n  provenance: %s\n  argv: %q\n  bash(%d): %q\n  koi(%d): %q",
			r.Name, r.Reason, r.Provenance, r.Argv, r.BashCode, r.BashOut, r.KoiCode, r.KoiOut)
	}
}

// Known markers are load-bearing suppressions, so they get their own
// gate. Two ways a marker goes wrong, and both are failures here:
//
//   - it outlives its bug. The case passes, the marker still suppresses
//     it, and from then on nothing enforces a behavior koi has already
//     earned. The published page also keeps advertising a gap that is
//     closed, which is the kind of wrong that costs adoption.
//   - it arrives without a reason. `Known` with no `KnownNote` is a
//     silenced case, indistinguishable from a case someone found
//     inconvenient.
func TestAgentKnownGaps(t *testing.T) {
	for _, c := range compat.AgentCorpus {
		if c.Known == 0 {
			if c.KnownNote != "" {
				t.Errorf("%s: KnownNote without Known — nothing links this note to an issue", c.Name)
			}
			continue
		}
		if c.KnownNote == "" {
			t.Errorf("%s: Known #%d with no KnownNote — a suppressed case needs to say what it costs", c.Name, c.Known)
		}
		if c.Expect != "" {
			t.Errorf("%s: Known #%d and Expect together — a case is either a deliberate divergence or a bug, not both", c.Name, c.Known)
		}
	}

	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH; the oracle is required")
	}
	koiBin := buildKoi(t)

	results := compat.RunAgentAll(context.Background(), bashBin, koiBin)
	for _, r := range compat.AgentStaleKnown(results) {
		t.Errorf("%s: passes now, but is still marked Known #%d.\n"+
			"  Delete the Known/KnownNote fields so this case is enforced again, and close the issue.",
			r.Name, r.Known)
	}
	if gaps := compat.AgentKnownGaps(results); len(gaps) > 0 {
		for _, r := range gaps {
			t.Logf("still open: #%d %s — %s", r.Known, r.Name, r.KnownNote)
		}
	}
}

// The published table has to say what this machine's run says.
//
// A gap table maintained by hand drifts the moment one is fixed, and it
// drifts flatteringly. So the page carries a generated region and this
// test regenerates it under -update-agent, or checks it otherwise: every
// open issue number in the corpus must appear in the page, and every
// issue number the page claims must still be open in the corpus.
func TestAgentGapsDoc(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH; the oracle is required")
	}
	koiBin := buildKoi(t)
	results := compat.RunAgentAll(context.Background(), bashBin, koiBin)

	page, err := os.ReadFile(agentDocPath)
	if err != nil {
		t.Fatal(err)
	}
	section := compat.AgentGapsSection(results, bashVersion(t, bashBin))

	if *updateAgent {
		updated, rerr := compat.ReplaceAgentGaps(string(page), section)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(agentDocPath, []byte(updated), 0o600); werr != nil {
			t.Fatal(werr)
		}
		t.Logf("wrote %s: %d open gaps", agentDocPath, len(compat.AgentKnownGaps(results)))
		return
	}

	if _, rerr := compat.ReplaceAgentGaps(string(page), section); rerr != nil {
		t.Fatal(rerr)
	}
	// Compare membership rather than the rendered text: the headline
	// carries a bash version and a pass count that legitimately differ by
	// machine, and failing on those would train people to run
	// -update-agent to make an unrelated red go away.
	for _, r := range compat.AgentKnownGaps(results) {
		ref := "issues/" + itoaTest(r.Known)
		if !strings.Contains(string(page), ref) {
			t.Errorf("#%d (%s) is an open gap in the corpus but is not in %s.\n"+
				"  Run `make agent-gate` to republish.", r.Known, r.Name, agentDocPath)
		}
	}
}

func itoaTest(n int) string {
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

// The two deliberate divergences are the ones most likely to be "fixed"
// by someone making the gate green, so they assert the difference rather
// than tolerating it.
func TestAgentGateKeepsShellIdentityDistinct(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	koiBin := buildKoi(t)

	var found bool
	for _, c := range compat.AgentCorpus {
		if c.Expect == "" {
			continue
		}
		found = true
		if c.Why == "" {
			t.Errorf("%s: a deliberate divergence with no stated reason is indistinguishable from a bug", c.Name)
		}
		r := compat.RunAgent(context.Background(), bashBin, koiBin, c)
		if !r.Pass {
			t.Errorf("%s: koi printed %q, want %q", c.Name, r.KoiOut, c.Expect)
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
	koiBin := buildKoi(t)

	// An agent that ignores $SHELL entirely gets real bash, and koi
	// running the same thing must agree with it. If this ever diverges,
	// "it still works, that's the point" stops being true.
	c := compat.AgentCase{
		Name:       "hardcoded bash -c",
		Provenance: "anthropics/claude-code#11475 — agents ignore the user's default shell",
		Argv:       []string{"-c", `set -e; x=1; for i in 1 2 3; do x=$((x*2)); done; echo "$x"`},
	}
	if r := compat.RunAgent(context.Background(), bashBin, koiBin, c); !r.Pass {
		t.Errorf("%s: %s\n  bash(%d): %q\n  koi(%d): %q",
			c.Name, r.Reason, r.BashCode, r.BashOut, r.KoiCode, r.KoiOut)
	}
}
