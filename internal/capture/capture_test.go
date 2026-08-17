//go:build unix

package capture

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// run executes a /bin/sh script with its stdout and stderr on the
// capture session, exactly as the shell's exec path would, and returns
// what was mirrored to the "terminal" and what was retained.
func run(t *testing.T, script string, cols, rows int) (mirrored, retained string) {
	t.Helper()
	var screen bytes.Buffer
	s, err := Start(&screen, cols, rows, DefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Stdout, cmd.Stderr = s.Slave(), s.Slave()
	// Own process group, as the job table does. No Ctty here: this test
	// has no controlling terminal to hand over, and the point of the
	// design is that capture does not touch it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !bytes.Contains([]byte(err.Error()), []byte("exit status")) && !errorsAs(err, &ee) {
			t.Fatalf("run: %v", err)
		}
	}
	out := s.Close()
	return screen.String(), string(out)
}

func errorsAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError) //nolint:errorlint // direct exec result
	if ok {
		*target = e
	}
	return ok
}

// The whole reason this uses a PTY instead of a pipe: the child must
// still believe it is on a terminal, or every well-behaved program
// switches to its dumb output mode.
func TestChildStillSeesATerminal(t *testing.T) {
	_, out := run(t, `[ -t 1 ] && echo IS_TTY || echo NOT_TTY`, 80, 24)
	if !strings.Contains(out, "IS_TTY") {
		t.Errorf("child did not see a terminal on stdout: %q", out)
	}
}

// A program that asks how wide its terminal is must get the real
// answer, or its output wraps at the wrong column.
//
// The assertion is made against the slave descriptor itself — the very
// fd the child receives as stdout — rather than through a helper
// program, because tools disagree about which descriptor to ask.
// `stty`, notably, reads *stdin*, which this design deliberately leaves
// as the real terminal; asserting through it would test the wrong fd
// and quietly pass for the wrong reason.
func TestTerminalSizeIsPropagated(t *testing.T) {
	var screen bytes.Buffer
	s, err := Start(&screen, 100, 40, DefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cols, rows, err := ptySize(s.Slave())
	if err != nil {
		t.Fatal(err)
	}
	if cols != 100 || rows != 40 {
		t.Errorf("child's stdout reports %dx%d, want 100x40", cols, rows)
	}
}

// Resizing mid-session is what SIGWINCH forwarding depends on: the
// child learns of the change from its controlling terminal, but its
// stdout has to agree about the new width.
func TestResizeTakesEffect(t *testing.T) {
	var screen bytes.Buffer
	s, err := Start(&screen, 80, 24, DefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Resize(120, 50)
	cols, rows, err := ptySize(s.Slave())
	if err != nil {
		t.Fatal(err)
	}
	if cols != 120 || rows != 50 {
		t.Errorf("after resize stdout reports %dx%d, want 120x50", cols, rows)
	}

	// A nonsense size is ignored rather than applied: a zero-width
	// terminal makes well-behaved programs do strange things.
	s.Resize(0, 0)
	if cols, rows, _ := ptySize(s.Slave()); cols != 120 || rows != 50 {
		t.Errorf("a zero size was applied: now %dx%d", cols, rows)
	}
}

// ptySize reads the window size the way a child program would.
func ptySize(f *os.File) (cols, rows int, err error) {
	ws, err := pty.GetsizeFull(f)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Cols), int(ws.Rows), nil
}

// Output has to reach the screen as well as the buffer — capture is a
// history feature, never a reason output goes missing.
//
// Both streams are pointed at the session here because that exercises
// the session itself. The *shell* only ever routes stdout through it
// (see internal/jobs); stderr stays on the real terminal so full-screen
// programs keep working.
func TestOutputIsMirroredAndRetained(t *testing.T) {
	mirrored, retained := run(t, `echo hello; echo world >&2`, 80, 24)
	for _, want := range []string{"hello", "world"} {
		if !strings.Contains(mirrored, want) {
			t.Errorf("screen missing %q: %q", want, mirrored)
		}
		if !strings.Contains(retained, want) {
			t.Errorf("capture missing %q: %q", want, retained)
		}
	}
}

// The tail is what matters — a failed build's error is at the end — so
// the ring keeps the most recent bytes and says it dropped the rest.
func TestRingKeepsTheTailAndReportsTruncation(t *testing.T) {
	var screen bytes.Buffer
	s, err := Start(&screen, 80, 24, 1024)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `i=0; while [ $i -lt 400 ]; do echo "line-$i"; i=$((i+1)); done; echo FINAL_MARKER`)
	cmd.Stdout, cmd.Stderr = s.Slave(), s.Slave()
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	out := string(s.Close())

	if len(out) > 1024 {
		t.Errorf("ring kept %d bytes, over its 1024 limit", len(out))
	}
	if !strings.Contains(out, "FINAL_MARKER") {
		t.Errorf("the tail was dropped; that is the part worth keeping: %q", out)
	}
	if strings.Contains(out, "line-0\n") {
		t.Error("the head survived, so the ring dropped from the wrong end")
	}
	if !s.Truncated() {
		t.Error("truncation was not reported, so a partial log would read as complete")
	}
}

// Closing must not lose the last writes. A command's final lines are
// exactly what a build-log capture exists for.
func TestCloseDrainsTheTail(t *testing.T) {
	for range 20 { // a race shows up under repetition or not at all
		_, out := run(t, `printf 'a\nb\nc\nLAST\n'`, 80, 24)
		if !strings.Contains(out, "LAST") {
			t.Fatalf("lost the final line on close: %q", out)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	var screen bytes.Buffer
	s, err := Start(&screen, 80, 24, DefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	first := s.Close()
	second := s.Close() // must not panic or hang
	if len(first) != len(second) {
		t.Errorf("second Close returned different output")
	}
}

// A session nobody writes to still closes cleanly — the empty-command
// case, and the case where a builtin ran instead of a child.
func TestEmptySessionCloses(t *testing.T) {
	var screen bytes.Buffer
	s, err := Start(&screen, 80, 24, DefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	if out := s.Close(); len(out) != 0 {
		t.Errorf("captured %q from nothing", out)
	}
}

// Binary output must survive the round trip: a capture that mangles
// bytes would corrupt what it replays.
func TestBinarySafe(t *testing.T) {
	_, out := run(t, `printf 'A\000B\377C'`, 80, 24)
	if !strings.Contains(out, "A") || !strings.Contains(out, "C") {
		t.Errorf("binary payload mangled: %q", out)
	}
}

func TestSlaveIsAFile(t *testing.T) {
	var screen bytes.Buffer
	s, err := Start(&screen, 80, 24, DefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The exec path requires *os.File for stdio; anything else falls
	// through to the uncaptured handler, so Slave's type is part of the
	// contract rather than an implementation detail.
	if s.Slave() == nil {
		t.Fatal("slave is not a file")
	}
}

// A descriptor left open by something other than the command — a
// background job that inherited it, a daemon the command spawned —
// must not hang the shell. Close bounds its wait and gives up.
func TestCloseDoesNotHangOnALingeringHolder(t *testing.T) {
	var screen bytes.Buffer
	s, err := Start(&screen, 80, 24, DefaultLimit)
	if err != nil {
		t.Fatal(err)
	}

	// A child that outlives the line and keeps the slave open.
	lingering := exec.Command("/bin/sh", "-c", "sleep 30")
	lingering.Stdout = s.Slave()
	if err := lingering.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = lingering.Process.Kill()
		_, _ = lingering.Process.Wait()
	})

	done := make(chan []byte, 1)
	go func() { done <- s.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a lingering slave holder; the shell would be wedged")
	}
}
