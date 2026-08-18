//go:build unix

package compat_test

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// -update-suite regenerates docs/bash-suite.md from a live run.
var updateSuite = flag.Bool("update-suite", false, "regenerate docs/bash-suite.md")

const bashSuitePath = "../../docs/bash-suite.md"

// The bash suite is fetched, never committed (GPLv3 vs MIT — see
// bashsuite.go), so this skips loudly when it is absent rather than
// quietly passing. A gate that reads as green while testing nothing is
// the failure mode the whole scoreboard exists to avoid.
func TestBashSuite(t *testing.T) {
	// Opt-in, like the pty gates: 83 files through two shells is two
	// minutes, and much more under -race. A developer who has run
	// `make bash-suite` once must not pay that on every `make test`,
	// which is exactly what happened before this guard existed.
	if os.Getenv(gatesEnv) == "" {
		t.Skipf("set %s=1 to run bash's own suite (make bash-suite)", gatesEnv)
	}
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this machine: the differential oracle is unavailable")
	}
	dir, ok := compat.BashSuiteDir("../..")
	if !ok {
		t.Skipf("bash's tests/ not fetched (looked in %s) — run `make bash-suite`", dir)
	}
	helpers := filepath.Join("..", "..", "build", "bash-helpers")
	if _, err := os.Stat(filepath.Join(helpers, "recho")); err != nil {
		t.Skipf("bash's test helpers are not built (%s) — run `make bash-suite`", helpers)
	}
	abs, err := filepath.Abs(helpers)
	if err != nil {
		t.Fatal(err)
	}
	koiBin := buildKoi(t)

	results, err := compat.RunBashSuite(context.Background(), bashBin, koiBin, dir, abs)
	if err != nil {
		t.Fatal(err)
	}
	s := compat.SummarizeSuite(results)
	t.Logf("strict %d/%d (%d%%), parsed %d/%d (%d%%), lines %d/%d (%d%%)",
		s.Passed, s.Files, s.Rate(), s.Parsed, s.Files, s.ParseRate(),
		s.MatchedLine, s.BashLines, s.LineRate())

	if *updateSuite {
		// Read the previous run before overwriting it, so the delta is
		// produced rather than left for whoever writes the release note to
		// diff two tables by eye. That cadence is the whole point of the
		// move (#211): visible, dated, quantified progress.
		prev := compat.SuiteSummary{}
		if old, rerr := os.ReadFile(bashSuitePath); rerr == nil {
			prev = compat.ParsePublishedSuite(string(old))
		}
		doc := compat.BashSuiteDoc(results, bashVersion(t, bashBin), suiteVersion())
		if err := os.WriteFile(bashSuitePath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", bashSuitePath)
		t.Logf("RELEASE NOTE LINE: %s", compat.SuiteDelta(prev, s))
		return
	}

	// Not a pass-rate gate: this suite is run on demand and the number is
	// published rather than enforced (the corpus in compat.md is the
	// CI-gated one). What is asserted is that the harness still works —
	// a run where nothing parses or bash produced no output means the
	// helpers or the fetch broke, and that must not read as "koi got
	// worse".
	// Report the drift even though nothing here enforces it. docs/bash-suite.md
	// went eleven commits stale before anyone noticed, because a number that is
	// published rather than gated is a number nothing ever reads back. This does
	// not turn the suite into a pass-rate gate — that remains a deliberate no
	// (#274) — it just makes the delta visible in the log of every run, so
	// drifting from the published page is something you see rather than
	// something you discover a month later.
	if old, rerr := os.ReadFile(bashSuitePath); rerr == nil {
		if delta := compat.SuiteDelta(compat.ParsePublishedSuite(string(old)), s); delta != "" {
			t.Logf("VS PUBLISHED: %s — run `make bash-suite` to republish", delta)
		}
	}

	if s.BashLines == 0 {
		t.Fatal("bash produced no output for any file: the suite or its helpers are broken, not koi")
	}
	if s.Parsed == 0 {
		t.Error("koi parsed none of the suite: that is a harness failure, not a compatibility result")
	}
}

// suiteVersion reports which bash tarball is unpacked, so the doc says
// what it measured against rather than implying "whatever was current".
func suiteVersion() string {
	if v := os.Getenv("BASH_SUITE_VERSION"); v != "" {
		return v
	}
	return "5.3"
}

// The three measures answer different questions and must not be
// collapsed. This pins the arithmetic so a later refactor cannot quietly
// turn the strict number into the forgiving one.
func TestSuiteSummaryMeasuresStayDistinct(t *testing.T) {
	results := []compat.SuiteResult{
		{File: "a.tests", Pass: true, Parsed: true, BashLines: 10, Matched: 10},
		{File: "b.tests", Parsed: true, BashLines: 10, Matched: 5},
		{File: "c.tests", ParseError: "`++` must follow a name", BashLines: 10, Matched: 0},
		{File: "d.tests", ParseError: "`++` must follow a name", BashLines: 10, Matched: 1},
	}
	s := compat.SummarizeSuite(results)
	if s.Rate() != 25 {
		t.Errorf("strict rate = %d%%, want 25%% (1 of 4 files)", s.Rate())
	}
	if s.ParseRate() != 50 {
		t.Errorf("parse rate = %d%%, want 50%% (2 of 4 files)", s.ParseRate())
	}
	if s.LineRate() != 40 {
		t.Errorf("line rate = %d%%, want 40%% (16 of 40 lines)", s.LineRate())
	}

	// Gaps rank by how many files they cost, because that is the order
	// worth fixing them in.
	gaps := compat.ParseGaps(results)
	if len(gaps) != 1 {
		t.Fatalf("got %d gaps, want 1", len(gaps))
	}
	if len(gaps[0].Files) != 2 {
		t.Errorf("gap covers %d files, want 2", len(gaps[0].Files))
	}
}

// A doc that omits the strict number, or quietly reports only the
// friendliest one, is the failure this whole page exists to prevent.
func TestBashSuiteDocLeadsWithTheStrictNumber(t *testing.T) {
	results := []compat.SuiteResult{
		{File: "a.tests", Pass: true, Parsed: true, BashLines: 10, Matched: 10},
		{File: "b.tests", ParseError: "`++` must follow a name", BashLines: 90, Matched: 9},
	}
	doc := compat.BashSuiteDoc(results, "bash 5.3.15(1)-release", "5.3")
	for _, want := range []string{
		"**1/2 files (50%)**", // strict, bolded
		"1/2 files (50%)",     // parsed
		"19/100 lines (19%)",  // line agreement
		"`++` must follow a name",
		"GPLv3",
		"not a CI gate",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("generated doc does not contain %q", want)
		}
	}
}

// The delta is only useful if it survives a round trip through the
// published page — that page is the record of the last run, and there is
// no separate state file that could disagree with it.
func TestPublishedSuiteRoundTrips(t *testing.T) {
	results := []compat.SuiteResult{
		{File: "a.tests", Pass: true, Parsed: true, BashLines: 10, Matched: 10},
		{File: "b.tests", ParseError: "`++` must follow a name", BashLines: 90, Matched: 9},
	}
	doc := compat.BashSuiteDoc(results, "bash 5.3.15(1)-release", "5.3")
	got := compat.ParsePublishedSuite(doc)
	want := compat.SummarizeSuite(results)
	if got.Passed != want.Passed || got.Files != want.Files ||
		got.Parsed != want.Parsed || got.MatchedLine != want.MatchedLine || got.BashLines != want.BashLines {
		t.Errorf("round trip lost numbers:\n got %+v\nwant %+v", got, want)
	}

	// A page that has never been written must read as a first run rather
	// than as a regression from zero.
	if d := compat.SuiteDelta(compat.ParsePublishedSuite("nothing here"), want); !strings.Contains(d, "first run") {
		t.Errorf("delta from an unreadable page = %q, want a first-run line", d)
	}
}

// A parse error from a snippet the test deliberately fed to $THIS_SH is
// the test working, not koi failing to read the file. Conflating them
// understated the parse rate by nine files before this was pinned.
func TestParseErrorOnlyCountsTheFileUnderTest(t *testing.T) {
	// bash's suite runs broken input through the shell on purpose; the
	// error names the snippet, not the .tests file.
	subshell := "koi: ./tmp-snippet.sh:3:1: `if` must be followed by a statement list\nok 1\n"
	if got := compat.ParseErrorFor("arith.tests", subshell); got != "" {
		t.Errorf("charged a sub-invocation error to the file under test: %q", got)
	}

	// An error naming the file itself is the real thing: koi stopped
	// reading, and every later assertion in the file is forfeit.
	own := "koi: ./arith.tests:129:16: `+=` must follow a name\n"
	if got := compat.ParseErrorFor("arith.tests", own); got != "`+=` must follow a name" {
		t.Errorf("missed the file's own parse error: %q", got)
	}

	// Both present: the file's own error still wins, whichever came first.
	both := subshell + own
	if got := compat.ParseErrorFor("arith.tests", both); got == "" {
		t.Error("a real parse error was hidden by an earlier sub-invocation error")
	}
	if got := compat.ParseErrorFor("arith.tests", "clean output\n"); got != "" {
		t.Errorf("invented a parse error: %q", got)
	}
}
