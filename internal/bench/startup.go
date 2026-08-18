// Package bench measures koi against the world (#102): time to first
// prompt across shells and configurations, and keystroke-to-paint
// latency percentiles. Numbers are the argument in this market, so the
// harness is reproducible and the methodology is published beside the
// results.
//
// Fairness rules the harness enforces:
//   - every shell is started under a real pty, the same way;
//   - every shell is given a temp rc that sets a unique prompt marker,
//     so "first prompt" means the same event everywhere;
//   - a shell that is not installed is *reported as missing*, never
//     estimated or omitted silently.
package bench

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/creack/pty"
)

// marker is the prompt every configuration is made to print, so
// time-to-first-prompt is one comparable event.
const marker = "READYPROMPT>"

// startupTimeout bounds one launch; a shell that never prompts is a
// data point, not a hang.
const startupTimeout = 15 * time.Second

// Config is one measured shell setup.
type Config struct {
	// Label is the published row name.
	Label string
	// Bin is the shell binary; empty means "not installed" and the row
	// is reported as unavailable.
	Bin string
	// Args and Env start the shell; RC is written to a temp file the
	// config can reference through Env/Args via the {{rc}} placeholder.
	Args []string
	Env  []string
	RC   string
	// Note explains what the row includes — the honesty column.
	Note string
}

// StartupResult is one config's timings.
type StartupResult struct {
	Config
	Runs      []time.Duration
	Available bool
	Err       string
}

// Median returns the middle timing.
func (r StartupResult) Median() time.Duration {
	if len(r.Runs) == 0 {
		return 0
	}
	sorted := slices.Clone(r.Runs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// Best returns the fastest timing — the number a shell's own docs
// would quote, published beside the median so both are visible.
func (r StartupResult) Best() time.Duration {
	if len(r.Runs) == 0 {
		return 0
	}
	return slices.Min(r.Runs)
}

// MeasureStartup runs one config n times and returns its timings.
func MeasureStartup(cfg Config, n int) StartupResult {
	res := StartupResult{Config: cfg}
	if cfg.Bin == "" {
		res.Err = "not installed"
		return res
	}
	res.Available = true

	dir, err := os.MkdirTemp("", "koi-bench")
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer os.RemoveAll(dir) //nolint:errcheck // temp dir

	// The rc goes to every name a shell might look for under the temp
	// home: {{rc}} for explicit --rcfile, .zshrc for ZDOTDIR, .bashrc
	// for completeness. Only the one the shell reads takes effect.
	rcPath := filepath.Join(dir, "rc")
	for _, name := range []string{"rc", ".zshrc", ".bashrc", ".zshenv"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(cfg.RC), 0o600); err != nil {
			res.Err = err.Error()
			return res
		}
	}

	for range n {
		d, err := timeToPrompt(cfg, rcPath, dir)
		if err != nil {
			res.Err = err.Error()
			return res
		}
		res.Runs = append(res.Runs, d)
	}
	return res
}

// timeToPrompt launches the shell under a pty and measures until the
// marker prompt appears.
func timeToPrompt(cfg Config, rcPath, home string) (time.Duration, error) {
	args := make([]string, len(cfg.Args))
	for i, a := range cfg.Args {
		args[i] = expandPlaceholders(a, rcPath, home)
	}
	env := make([]string, len(cfg.Env))
	for i, e := range cfg.Env {
		env[i] = expandPlaceholders(e, rcPath, home)
	}

	cmd := exec.Command(cfg.Bin, args...) //nolint:gosec // benchmark configs are ours
	cmd.Env = env
	// Start in the temp home: a shell must not be charged for whatever
	// the measuring directory happens to contain (this repo's
	// .tool-versions, a huge git tree, a direnv file).
	cmd.Dir = home
	start := time.Now()
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		return 0, err
	}
	defer func() {
		f.Close()
		_ = cmd.Process.Kill()    //nolint:errcheck // teardown
		_, _ = cmd.Process.Wait() //nolint:errcheck // teardown
	}()

	// Read from a goroutine and time out in a select: SetReadDeadline
	// is unsupported on a pty ("file type does not support deadline"),
	// so a bare Read blocks forever once a shell goes quiet.
	chunks := readerChan(f)
	var buf bytes.Buffer
	deadline := time.After(startupTimeout)
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return 0, fmt.Errorf("shell exited before prompting (got %q)", truncate(buf.String(), 120))
			}
			buf.Write(chunk)
			if bytes.Contains(buf.Bytes(), []byte(marker)) {
				return time.Since(start), nil
			}
		case <-deadline:
			return 0, fmt.Errorf("no prompt within %s (got %q)", startupTimeout, truncate(buf.String(), 120))
		}
	}
}

// readerChan streams a pty's output; the channel closes when the pty
// does. One goroutine per launch, ended by closing the pty.
func readerChan(f *os.File) <-chan []byte {
	ch := make(chan []byte, 64)
	go func() {
		defer close(ch)
		for {
			chunk := make([]byte, 4096)
			n, err := f.Read(chunk)
			if n > 0 {
				ch <- chunk[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func expandPlaceholders(s, rcPath, home string) string {
	s = strings.ReplaceAll(s, "{{rc}}", rcPath)
	return strings.ReplaceAll(s, "{{home}}", home)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
