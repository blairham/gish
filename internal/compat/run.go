package compat

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// runTimeout bounds one case; a hang is a failure, not a stall.
const runTimeout = 20 * time.Second

// Result is one case's verdict. The comparison is differential: the
// same script runs under bash and under koi, and the outputs must
// agree. Nothing here encodes what bash "should" print — bash on the
// running machine is the oracle.
type Result struct {
	Case
	BashOut, KoiOut   string
	BashCode, KoiCode int
	Pass              bool
	Reason            string // why it failed, for the published table
}

// Run executes one case under both shells and compares.
func Run(ctx context.Context, bashBin, koiBin string, c Case) Result {
	r := Result{Case: c}
	r.BashOut, r.BashCode = runScript(ctx, bashBin, c.Script)
	r.KoiOut, r.KoiCode = runScript(ctx, koiBin, c.Script)

	switch {
	case r.BashOut == r.KoiOut && r.BashCode == r.KoiCode:
		r.Pass = true
	case r.BashOut != r.KoiOut && r.BashCode != r.KoiCode:
		r.Reason = "output and exit status differ"
	case r.BashOut != r.KoiOut:
		r.Reason = "output differs"
	default:
		r.Reason = "exit status differs"
	}
	return r
}

// RunAll runs the whole corpus.
func RunAll(ctx context.Context, bashBin, koiBin string) []Result {
	out := make([]Result, 0, len(Corpus))
	for _, c := range Corpus {
		out = append(out, Run(ctx, bashBin, koiBin, c))
	}
	return out
}

// runScript runs one script through a shell's -c and returns combined
// output plus exit status. Combined because a difference in *where*
// output lands is itself a compat difference worth catching.
func runScript(ctx context.Context, shell, script string) (string, int) {
	rctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, shell, "-c", script) //nolint:gosec // fixed shells, curated corpus
	// A hermetic-ish environment: the corpus must not depend on the
	// runner's dotfiles or locale.
	cmd.Env = []string{"PATH=" + pathEnv(), "HOME=" + homeEnv(), "LC_ALL=C", "TERM=dumb"}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		return buf.String() + "\n[runner error: " + err.Error() + "]", -1
	}
	return buf.String(), code
}

func pathEnv() string   { return os.Getenv("PATH") }
func homeEnv() string   { return os.Getenv("HOME") }
func itoa(n int) string { return strconv.Itoa(n) }

// Summary is the scoreboard's headline numbers.
type Summary struct {
	Total, Passed int
	ByCategory    map[Category][2]int // category → {passed, total}
}

// Summarize tallies results by category.
func Summarize(results []Result) Summary {
	s := Summary{ByCategory: map[Category][2]int{}}
	for _, r := range results {
		s.Total++
		counts := s.ByCategory[r.Category]
		counts[1]++
		if r.Pass {
			s.Passed++
			counts[0]++
		}
		s.ByCategory[r.Category] = counts
	}
	return s
}

// Rate is the pass percentage, rounded down — a scoreboard should
// never round in its own favor.
func (s Summary) Rate() int {
	if s.Total == 0 {
		return 0
	}
	return s.Passed * 100 / s.Total
}

// Categories returns the corpus categories in declaration order, so
// the published table is stable across runs.
func Categories() []Category {
	var out []Category
	for _, c := range Corpus {
		if !slices.Contains(out, c.Category) {
			out = append(out, c.Category)
		}
	}
	return out
}

// Failures returns the failing results, corpus order.
func Failures(results []Result) []Result {
	var out []Result
	for _, r := range results {
		if !r.Pass {
			out = append(out, r)
		}
	}
	return out
}

// Diff renders a compact one-line difference for the published table.
func (r Result) Diff() string {
	if r.Pass {
		return ""
	}
	bash := strings.TrimSpace(r.BashOut)
	koi := strings.TrimSpace(r.KoiOut)
	if bash == koi {
		return "exit " + itoa(r.BashCode) + " vs " + itoa(r.KoiCode)
	}
	return "bash: " + firstLine(bash) + " · koi: " + firstLine(koi)
}

func firstLine(s string) string {
	if s == "" {
		return "(no output)"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	if len(s) > 60 {
		s = s[:57] + "…"
	}
	return s
}
