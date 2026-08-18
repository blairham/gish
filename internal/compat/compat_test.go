package compat_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// -update regenerates docs/compat.md from a live run; without it the
// test is the CI regression gate (#101): the corpus must not pass
// fewer cases than the published scoreboard records.
var update = flag.Bool("update", false, "regenerate docs/compat.md")

const scoreboardPath = "../../docs/compat.md"

// recordedRe pulls the pass count out of the published headline.
var recordedRe = regexp.MustCompile(`\*\*(\d+) of (\d+) cases pass`)

// recordedBashRe pulls the oracle version the scoreboard was generated
// against. The pass count is only comparable against the same bash
// major: macOS ships bash 3.2 (2007), where *bash itself* rejects
// `${s,,}` and `declare -A` and koi is ahead of the oracle.
var recordedBashRe = regexp.MustCompile(`against bash (\d+)\.`)

func TestCompatScoreboard(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this machine: the differential oracle is unavailable")
	}
	koiBin := buildKoi(t)

	results := compat.RunAll(context.Background(), bashBin, koiBin)
	summary := compat.Summarize(results)

	if *update {
		doc := compat.Scoreboard(results, bashVersion(t, bashBin))
		if err := os.WriteFile(scoreboardPath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s: %d/%d (%d%%)", scoreboardPath, summary.Passed, summary.Total, summary.Rate())
		return
	}

	// Regression gate: never fewer passes than published — but only
	// against the same bash major the scoreboard was generated with.
	recorded, recordedTotal := readRecorded(t)
	if got, want := bashMajor(t, bashBin), recordedBashMajor(t); got != want {
		t.Skipf("oracle is bash %s, scoreboard was generated against bash %s: "+
			"pass counts are not comparable across majors (this run: %d/%d)",
			got, want, summary.Passed, summary.Total)
	}
	if summary.Passed < recorded {
		for _, f := range compat.Failures(results) {
			t.Logf("FAIL %s (%s): %s — %s", f.Name, f.Category, f.Reason, f.Diff())
		}
		t.Fatalf("compat regression: %d/%d passing, scoreboard records %d/%d — run `make compat` after fixing",
			summary.Passed, summary.Total, recorded, recordedTotal)
	}
	if summary.Passed > recorded {
		t.Logf("compat improved: %d passing vs %d recorded — run `make compat` to publish",
			summary.Passed, recorded)
	}
}

// TestCorpusIsWellFormed guards the corpus itself: a scoreboard whose
// cases are duplicated or unattributed cannot be argued with.
func TestCorpusIsWellFormed(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, c := range compat.Corpus {
		if seen[c.Name] {
			t.Errorf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
		if c.Provenance == "" {
			t.Errorf("case %q has no provenance", c.Name)
		}
		if strings.TrimSpace(c.Script) == "" {
			t.Errorf("case %q has an empty script", c.Name)
		}
		if c.Category == "" {
			t.Errorf("case %q has no category", c.Name)
		}
	}
}

func buildKoi(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "koi")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/koi")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build koi: %v\n%s", err, out)
	}
	return bin
}

// bashMajor is the oracle's major version ("5"), or "?" when unknown.
func bashMajor(t *testing.T, bashBin string) string {
	t.Helper()
	v := bashVersion(t, bashBin)
	if i := strings.Index(v, "bash "); i >= 0 {
		v = v[i+len("bash "):]
	}
	if i := strings.IndexByte(v, '.'); i > 0 {
		return v[:i]
	}
	return "?"
}

// recordedBashMajor is the major version the scoreboard records.
func recordedBashMajor(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(scoreboardPath)
	if err != nil {
		return "?"
	}
	if m := recordedBashRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return "?"
}

func bashVersion(t *testing.T, bashBin string) string {
	t.Helper()
	out, err := exec.Command(bashBin, "-c", "echo $BASH_VERSION").Output()
	if err != nil {
		return "bash (version unknown)"
	}
	return "bash " + strings.TrimSpace(string(out))
}

func readRecorded(t *testing.T) (passed, total int) {
	t.Helper()
	data, err := os.ReadFile(scoreboardPath)
	if err != nil {
		t.Fatalf("%s missing — run `make compat` to create it: %v", scoreboardPath, err)
	}
	m := recordedRe.FindSubmatch(data)
	if m == nil {
		t.Fatalf("%s has no headline count", scoreboardPath)
	}
	passed, _ = strconv.Atoi(string(m[1])) //nolint:errcheck // regex guarantees digits
	total, _ = strconv.Atoi(string(m[2]))  //nolint:errcheck // as above
	return passed, total
}

// TestScoreboardMatchesCorpusSize catches the doc drifting from the
// corpus without a regeneration.
func TestScoreboardMatchesCorpusSize(t *testing.T) {
	data, err := os.ReadFile(scoreboardPath)
	if err != nil {
		t.Skipf("no scoreboard yet: %v", err)
	}
	m := recordedRe.FindSubmatch(data)
	if m == nil {
		t.Fatal("scoreboard has no headline count")
	}
	total, _ := strconv.Atoi(string(m[2])) //nolint:errcheck // regex guarantees digits
	if total != len(compat.Corpus) {
		t.Errorf("scoreboard covers %d cases, corpus has %d — run `make compat`",
			total, len(compat.Corpus))
	}
	fmt.Fprintln(os.Stderr) // keep -v output readable
}
