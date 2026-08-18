package compat

import (
	"fmt"
	"sort"
	"strings"
)

// The generated half of docs/agents.md (#208).
//
// The prose on that page is written by hand; this replaces one marked
// region inside it with what the gate actually reports. The split matters:
// the claim is an argument someone has to make, and the scoreboard is a
// measurement nobody should be able to round up. A hand-maintained table
// of open gaps drifts the moment one is fixed, and the direction it drifts
// is always flattering.
//
// Known gaps are published rather than kept in the issue tracker alone,
// for the same reason docs/bash-suite.md publishes its parse failures: a
// compatibility page that lists only passes is not a compatibility page.

const (
	// AgentGapsBegin and AgentGapsEnd delimit the generated region.
	AgentGapsBegin = "<!-- BEGIN generated agent gaps -->"
	AgentGapsEnd   = "<!-- END generated agent gaps -->"
)

// AgentGapsSection renders the generated region: a one-line verdict, then
// the open gaps grouped by issue.
func AgentGapsSection(results []AgentResult, bashVersion string) string {
	var b strings.Builder
	b.WriteString(AgentGapsBegin)
	b.WriteString("\n\n")

	var pass, skipped int
	for _, r := range results {
		switch {
		case r.Skipped:
			skipped++
		case r.Pass:
			pass++
		}
	}
	gaps := AgentKnownGaps(results)
	regressions := AgentRegressions(results)

	// bashVersion already carries its own "bash" prefix on some paths;
	// don't print it twice.
	oracle := strings.TrimSpace(bashVersion)
	if !strings.HasPrefix(oracle, "bash") {
		oracle = "bash " + oracle
	}
	fmt.Fprintf(&b, "**%d of %d cases agree with %s.** %d open gap%s, %d unfiled failure%s",
		pass, len(results), oracle,
		len(gaps), plural(len(gaps)), len(regressions), plural(len(regressions)))
	if skipped > 0 {
		fmt.Fprintf(&b, ", %d skipped for an oracle too old to answer them", skipped)
	}
	b.WriteString(".\n")

	if len(regressions) > 0 {
		b.WriteString("\nAn unfiled failure is a regression and fails CI; it appears here only if this page was regenerated from a red run.\n")
		for _, r := range regressions {
			fmt.Fprintf(&b, "\n- **%s** — %s\n", escapePipes(r.Name), escapePipes(r.Reason))
		}
	}

	if len(gaps) == 0 {
		b.WriteString("\nNo open gaps: every case an agent's shell hits agrees with bash.\n")
	} else {
		b.WriteString("\nEach of these is filed, reproduced, and failing right now. " +
			"They are suppressed in CI by an issue number in the corpus, and the " +
			"suppression is itself gated — a case that starts passing while still " +
			"marked fails the build, so a fix cannot land without updating this table.\n\n")
		b.WriteString("| issue | case | what it costs |\n| --- | --- | --- |\n")
		for _, r := range sortedByIssue(gaps) {
			fmt.Fprintf(&b, "| [#%d](https://github.com/blairham/koi-shell/issues/%d) | %s | %s |\n",
				r.Known, r.Known, escapePipes(r.Name), escapePipes(r.KnownNote))
		}
		b.WriteString("\nThe shape they share is not a missing feature — it is a missing " +
			"feature that **reports success**. A harness cannot route around a " +
			"failure it is never told about, which is what makes this class " +
			"expensive out of proportion to its size.\n")
	}

	b.WriteString("\n")
	b.WriteString(AgentGapsEnd)
	return b.String()
}

// ReplaceAgentGaps swaps the generated region in an existing page, leaving
// the hand-written prose around it untouched. A page missing its markers
// is an error rather than an append: silently adding a second scoreboard
// is how a doc ends up with two answers.
func ReplaceAgentGaps(page, section string) (string, error) {
	start := strings.Index(page, AgentGapsBegin)
	end := strings.Index(page, AgentGapsEnd)
	if start < 0 || end < 0 || end < start {
		return "", fmt.Errorf("docs/agents.md is missing the %q / %q markers",
			AgentGapsBegin, AgentGapsEnd)
	}
	return page[:start] + section + page[end+len(AgentGapsEnd):], nil
}

func sortedByIssue(in []AgentResult) []AgentResult {
	out := make([]AgentResult, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Known != out[j].Known {
			return out[i].Known < out[j].Known
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
