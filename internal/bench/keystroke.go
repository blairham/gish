package bench

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"time"

	"github.com/creack/pty"
)

// Keystroke latency (#102): the number the "you put RPC in my
// keystroke path?" question demands. Measured end to end — a byte goes
// into the pty, and the clock stops when the shell's repaint comes
// back out — so it includes decode, buffer edit, highlight, suggestion
// lookup, and render. Nothing is instrumented inside the process,
// because the honest question is what the terminal sees.

// keystrokeTimeout bounds one keypress; a miss is recorded, not
// retried, so a stall cannot hide inside an average.
const keystrokeTimeout = 2 * time.Second

// KeystrokeResult is one scenario's latency distribution.
type KeystrokeResult struct {
	Scenario string
	Note     string
	Samples  []time.Duration
	Err      string
}

// P50 is the median keystroke latency.
func (r KeystrokeResult) P50() time.Duration { return r.percentile(50) }

// P99 is the tail — the number that decides whether a shell "feels
// laggy", since the worst keystroke is the one users remember.
func (r KeystrokeResult) P99() time.Duration { return r.percentile(99) }

func (r KeystrokeResult) percentile(p int) time.Duration {
	if len(r.Samples) == 0 {
		return 0
	}
	sorted := slices.Clone(r.Samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted)-1)*p/100 + 0
	return sorted[idx]
}

// KeystrokeScenario describes one measured typing situation.
type KeystrokeScenario struct {
	Scenario string
	Note     string
	// Env configures the shell for this scenario (theme, lint, etc.).
	Env []string
	// Prefix is typed and discarded before measurement — it puts the
	// buffer in the state the scenario is about (e.g. a command name so
	// highlighting has work to do).
	Prefix string
	// Key is the byte measured, repeated Samples times.
	Key byte
}

// MeasureKeystrokes types into a live gish and records per-keypress
// latency for each scenario.
func MeasureKeystrokes(gishBin string, scenarios []KeystrokeScenario, samples int) []KeystrokeResult {
	out := make([]KeystrokeResult, 0, len(scenarios))
	for _, sc := range scenarios {
		out = append(out, measureOne(gishBin, sc, samples))
	}
	return out
}

func measureOne(gishBin string, sc KeystrokeScenario, samples int) KeystrokeResult {
	res := KeystrokeResult{Scenario: sc.Scenario, Note: sc.Note}

	home, err := os.MkdirTemp("", "gish-keys")
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer os.RemoveAll(home) //nolint:errcheck // temp dir

	env := append([]string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home + "/config",
		"XDG_DATA_HOME=" + home + "/data",
		"XDG_STATE_HOME=" + home + "/state",
		"TERM=xterm-256color",
		"PATH=" + os.Getenv("PATH"),
		"GISH_PROMPT=" + marker + " ",
	}, sc.Env...)

	cmd := exec.Command(gishBin) //nolint:gosec // our own binary
	cmd.Env = env
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer func() {
		f.Close()
		_ = cmd.Process.Kill()    //nolint:errcheck // teardown
		_, _ = cmd.Process.Wait() //nolint:errcheck // teardown
	}()

	chunks := readerChan(f)

	// Wait for the first prompt: startup cost must not contaminate
	// keystroke numbers.
	if err := waitFor(chunks, []byte(marker), startupTimeout); err != nil {
		res.Err = "no prompt: " + err.Error()
		return res
	}
	for _, b := range []byte(sc.Prefix) {
		if _, err := f.Write([]byte{b}); err != nil {
			res.Err = err.Error()
			return res
		}
		// Let the prefix settle so its repaints aren't measured.
		_ = waitForAny(chunks, 300*time.Millisecond) //nolint:errcheck // warm-up
	}
	drain(chunks)

	for range samples {
		start := time.Now()
		if _, err := f.Write([]byte{sc.Key}); err != nil {
			res.Err = err.Error()
			return res
		}
		if err := waitForAny(chunks, keystrokeTimeout); err != nil {
			res.Err = fmt.Sprintf("keystroke never painted: %v", err)
			return res
		}
		res.Samples = append(res.Samples, time.Since(start))
		drain(chunks) // trailing repaint bytes belong to this keystroke
	}
	return res
}

// waitFor consumes chunks until want appears or the timeout passes.
func waitFor(chunks <-chan []byte, want []byte, timeout time.Duration) error {
	var buf bytes.Buffer
	deadline := time.After(timeout)
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return fmt.Errorf("shell exited")
			}
			buf.Write(chunk)
			if bytes.Contains(buf.Bytes(), want) {
				return nil
			}
		case <-deadline:
			return fmt.Errorf("timeout after %s", timeout)
		}
	}
}

// waitForAny returns as soon as any byte arrives — the repaint.
func waitForAny(chunks <-chan []byte, timeout time.Duration) error {
	select {
	case _, ok := <-chunks:
		if !ok {
			return fmt.Errorf("shell exited")
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout after %s", timeout)
	}
}

// drain empties whatever has already arrived, so the next measurement
// starts from silence.
func drain(chunks <-chan []byte) {
	for {
		select {
		case <-chunks:
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}
