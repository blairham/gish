package compat

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	// A scratch $HOME, not the developer's (#260). This block said
	// "hermetic-ish" and the -ish was doing the work: `make compat`
	// writes the published scoreboard, so a case that reads anything
	// under $HOME was scored against whatever happened to be in one
	// person's home directory, and a case that *wrote* would have
	// written into it. Nothing in the corpus does either today, which is
	// exactly the state in which this gets missed.
	//
	// The two runners beside this one already did it right and are what
	// this copies: bashsuite.go and agent.go.
	home, err := os.MkdirTemp("", "koi-compat-")
	if err != nil {
		return "[runner error: " + err.Error() + "]", -1
	}
	defer os.RemoveAll(home) //nolint:errcheck // scratch dir

	rctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, shell, "-c", script) //nolint:gosec // fixed shells, curated corpus
	// TMPDIR is passed through rather than left unset: with no TMPDIR a
	// shell falls back to /tmp, so a case needing a temp file (process
	// substitution makes a fifo) fails wherever /tmp is not writable and
	// the scoreboard publishes a compat gap that is really a sandbox.
	// It deliberately stays the system one rather than following $HOME
	// into the scratch dir: what it has to be is *writable*, and the
	// system temp dir is the value both shells would have used anyway.
	cmd.Env = []string{
		"PATH=" + pathEnv(), "HOME=" + home, "LC_ALL=C", "TERM=dumb",
		"TMPDIR=" + os.TempDir(),
		// The XDG roots go inside it too, the way agent.go does: koi
		// resolves its rc through XDG before $HOME, so a scratch home
		// alone would still let a real ~/.config/koi decide the score.
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err = cmd.Run()
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
