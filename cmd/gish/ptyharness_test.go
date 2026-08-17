//go:build unix

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// One harness for every test that drives gish through a real pty.
//
// This exists because there were two, and they drifted. The raw-mode
// key-drop race was fixed in one and left in its twin, which cost a red
// main and a CI round to rediscover; the retry that fixed it was then
// written twice, wrongly the first time in both. A pattern duplicated
// across files is a pattern that will be fixed in one of them.
//
// Everything here was learned from a failure, and each rule is written
// where the next person will trip over it rather than in a commit
// message nobody reads.

// The two budgets below exist because "slow" and "stuck" are different
// failures and only one of them is a bug (#189).
//
// These drive a real shell through a real pty, and a busy CI runner
// redraws the prompt for every echoed keystroke — roughly a third of a
// second each. A single total-elapsed deadline cannot tell a shell that
// is answering slowly from one that has wedged, so it fails the first
// and calls it the second. That is what it did: `go test -race ./...`
// runs every package at once, and on a loaded runner these tests failed
// after reading *thousands* of bytes — visibly making progress, killed
// for taking too long in total. Five distinct tests failed that way in
// one afternoon, including on an unchanged `main`.
//
// So the primary bound is silence. e2eIdleBudget is how long the
// terminal may produce *nothing at all* before we conclude the shell is
// not coming back. A test that is being served slowly keeps resetting
// it and passes; a test whose shell has hung trips it, and does so
// faster than the old total bound did.
const e2eIdleBudget = 30 * time.Second

// e2eBudget then caps one wait outright. It is a backstop against
// livelock — output arriving forever without the marker ever appearing,
// which no idle bound can catch — so it is deliberately far longer than
// any honest wait. The cost of raising it is only how long a genuinely
// wedged test takes to report; the cost of lowering it is flakes, which
// is the more expensive mistake by a distance.
const e2eBudget = 4 * time.Minute

// retryQuiet is how long the terminal must be silent before a keystroke
// is assumed lost and resent. Resending on every chunk of output instead
// fires a key per burst of redraw, which walks straight through a form
// and out the other side.
const retryQuiet = 750 * time.Millisecond

// Waiting rule, learned three times over: a step must wait for
// something printed *last* by the line it sent, and the next keystroke
// must not go out before that arrives.
//
// Waiting on interesting-but-intermediate output returns while the line
// is still finishing, so the following send lands during the prompt
// redraw and is lost. That produced three separate CI-only failures —
// `jobs` waited on "Stopped" and then sent `bg`; a command was sent and
// then probed as a second line; a Ctrl-Z notice was treated as the end
// of a line. All three passed locally and failed on a slower runner.
//
// In practice: end each command line with `printf "res%s\n" NAME` and
// wait for that, or — where a keystroke cannot carry a marker, as with
// Ctrl-Z and Ctrl-C — wait for the fresh prompt with waitForPrompt.
//
// promptEnd is the OSC 133;B mark, written exactly when the prompt ends
// and input begins (#99 stage 1).
//
// Always wait for this rather than for the prompt's own characters. A CI
// runner turned out to have a 73-character hostname, which pushed the
// naked prompt past the pty width — the wrap fell between the "%" and
// its trailing space, so `" % "` never appeared contiguously and every
// test in the file failed on a machine where the shell was fine. A
// semantic mark cannot wrap: it is zero-width and atomic.
const promptEnd = "\x1b]133;B"

// ptySession is one gish process on a pty.
type ptySession struct {
	t   *testing.T
	f   *os.File
	buf *bytes.Buffer
	ch  <-chan []byte
	cmd *exec.Cmd
	raw *int64 // bytes read before any ANSI stripping
}

// ptyOptions configure a session.
type ptyOptions struct {
	// Args are gish's arguments; empty means an interactive shell.
	Args []string
	// Dir is the working directory; empty means the hermetic home.
	Dir string
	// Env adds to (and overrides) the hermetic environment.
	Env []string
	// Rows and Cols size the pty. Zero uses a wide default: few rows is
	// sometimes deliberate (so a pager pages), but narrow columns never
	// are — see promptEnd.
	Rows, Cols uint16
}

func startPTY(t *testing.T, opts ptyOptions) *ptySession {
	t.Helper()
	bin := buildGish(t)
	base := t.TempDir()
	dir := opts.Dir
	if dir == "" {
		dir = base
	}
	if opts.Rows == 0 {
		opts.Rows = 40
	}
	if opts.Cols == 0 {
		opts.Cols = 200
	}

	cmd := exec.Command(bin, opts.Args...)
	cmd.Env = append([]string{
		"HOME=" + base,
		"XDG_CONFIG_HOME=" + filepath.Join(base, "config"),
		"XDG_DATA_HOME=" + filepath.Join(base, "data"),
		"XDG_STATE_HOME=" + filepath.Join(base, "state"),
		"TERM=xterm-256color",
		"PATH=" + os.Getenv("PATH"),
	}, opts.Env...)
	cmd.Dir = dir

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: opts.Rows, Cols: opts.Cols})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Read in a goroutine and time out in a select: SetReadDeadline is
	// unsupported on a pty, so a bare Read blocks forever once the shell
	// goes quiet. Draining also matters in itself — a pty nobody reads
	// fills, and the child then blocks on write and looks hung.
	var raw int64
	ch := make(chan []byte, 64)
	go func() {
		defer close(ch)
		for {
			chunk := make([]byte, 8192)
			n, err := f.Read(chunk)
			if n > 0 {
				atomic.AddInt64(&raw, int64(n))
				ch <- chunk[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	return &ptySession{t: t, f: f, buf: &bytes.Buffer{}, ch: ch, cmd: cmd, raw: &raw}
}

// diagnose answers the two questions that separate the causes when a
// wait fails: did the shell write anything at all, and is it still
// alive? "got:" followed by nothing cannot distinguish a shell that
// never wrote from a matcher that missed what it wrote.
func (s *ptySession) diagnose() string {
	state := "still running"
	if p := s.cmd.ProcessState; p != nil {
		state = "exited: " + p.String()
	} else if s.cmd.Process != nil {
		if err := s.cmd.Process.Signal(syscall.Signal(0)); err != nil {
			state = "process gone: " + err.Error()
		}
	}
	return fmt.Sprintf("[%d raw bytes read, %s]", atomic.LoadInt64(s.raw), state)
}

// seen reports whether want is present, checking the raw bytes as well
// as the stripped text so callers may wait on escape sequences.
func (s *ptySession) seen(want string) bool {
	return bytes.Contains(s.buf.Bytes(), []byte(want)) ||
		bytes.Contains(ansiRe.ReplaceAll(s.buf.Bytes(), nil), []byte(want))
}

func (s *ptySession) plain() string {
	return string(ansiRe.ReplaceAll(s.buf.Bytes(), nil))
}

// waitFor blocks until want appears, and returns the stripped output.
//
// It gives up on silence rather than on elapsed time — see e2eIdleBudget.
func (s *ptySession) waitFor(want string) string {
	s.t.Helper()
	hard := time.After(e2eBudget)
	idle := time.NewTimer(e2eIdleBudget)
	defer idle.Stop()
	for {
		if s.seen(want) {
			return s.plain()
		}
		select {
		case chunk, ok := <-s.ch:
			if !ok {
				s.t.Fatalf("shell exited before %q %s; got:\n%s", want, s.diagnose(), s.plain())
			}
			s.buf.Write(chunk)
			// Progress: the shell is alive and answering, however
			// slowly. Go 1.23+ makes a bare Reset safe — a stale value
			// is never delivered after it.
			idle.Reset(e2eIdleBudget)
		case <-idle.C:
			s.t.Fatalf("no output for %s while waiting for %q %s; got:\n%s",
				e2eIdleBudget, want, s.diagnose(), s.plain())
		case <-hard:
			s.t.Fatalf("did not see %q within %s %s; got:\n%s", want, e2eBudget, s.diagnose(), s.plain())
		}
	}
}

// waitForPrompt waits for a prompt to finish rendering.
func (s *ptySession) waitForPrompt() string { s.t.Helper(); return s.waitFor(promptEnd) }

func (s *ptySession) send(keys string) {
	s.t.Helper()
	if _, err := s.f.WriteString(keys); err != nil {
		s.t.Fatal(err)
	}
}

// sendUntil sends keys and keeps sending them, on silence, until want
// appears.
//
// A program's output reaching the screen does not mean it is reading
// keys: entering raw mode discards whatever is already queued, so a
// keystroke sent in that window is lost. There is no portable signal for
// "listening now", and how wide the window is depends on the machine, so
// the honest contract is to send until the effect is observed. Every key
// driven this way must be idempotent.
//
// A write error means the program already exited — a reason to stop
// sending and check the marker, not a failure in itself.
//
// The buffer is cleared first, so want means "appeared after these keys"
// rather than "is somewhere in everything seen so far". Waiting on a
// marker that is already present returns immediately and sends the keys
// exactly once, which is precisely the retry this function exists to
// perform: `sendUntil("q", promptEnd)` was satisfied by the prompt that
// preceded the pager, so a `q` that landed before less reached raw mode
// was never resent, and the next command went to the pager instead of
// the shell. That failed only when the runner was slow enough to lose
// the race — a flake whose cause is invisible from the symptom.
func (s *ptySession) sendUntil(keys, want string) {
	s.t.Helper()
	s.buf.Reset()
	deadline := time.After(e2eBudget)
	alive := true
	send := func() {
		if alive {
			if _, err := s.f.WriteString(keys); err != nil {
				alive = false
			}
		}
	}
	send()
	quiet := time.After(retryQuiet)
	idle := time.NewTimer(e2eIdleBudget)
	defer idle.Stop()
	for {
		if s.seen(want) {
			return
		}
		select {
		case chunk, ok := <-s.ch:
			if !ok {
				s.t.Fatalf("shell exited before %q %s; got:\n%s", want, s.diagnose(), s.plain())
			}
			s.buf.Write(chunk)
			idle.Reset(e2eIdleBudget)
		case <-quiet:
			send()
			quiet = time.After(retryQuiet)
		case <-idle.C:
			// Silence here is louder than in waitFor: the keys have
			// been resent every retryQuiet the whole time and the
			// terminal has still said nothing back.
			s.t.Fatalf("no output for %s while resending %q and waiting for %q %s; got:\n%s",
				e2eIdleBudget, keys, want, s.diagnose(), s.plain())
		case <-deadline:
			s.t.Fatalf("did not see %q within %s %s; got:\n%s", want, e2eBudget, s.diagnose(), s.plain())
		}
	}
}

// stepUntil waits for a field to be live, then sends keys until the next
// expected thing appears.
func (s *ptySession) stepUntil(ready, keys, next string) {
	s.t.Helper()
	s.waitFor(ready)
	s.sendUntil(keys, next) // clears the buffer itself
}

// probe runs a command whose output cannot be confused with the echo of
// the command itself.
//
// The terminal echoes what is typed, so a sentinel chosen for the output
// usually also matches the echoed command. `printf 'res%s\n' NAME`
// produces "resNAME" only in the output, never in the command text.
//
// The leading Ctrl-U discards whatever is already on the line, because a
// probe usually follows sendUntil, whose retry cannot be idempotent for
// every key it is given. Quitting a pager is the case that matters: `q`
// is harmless while less is up and a literal character the moment it is
// not, so a retry that arrives just after the pager exits leaves a `q`
// on the line and turns the next command into `qprintf …`. The probe
// then waits for output that a command-not-found will never produce.
// Clearing first makes the probe independent of what came before it,
// which is the only version of this that stays true as callers change.
func (s *ptySession) probe(name string) { s.send("\x15printf 'res%s\\n' " + name + "\r") }
