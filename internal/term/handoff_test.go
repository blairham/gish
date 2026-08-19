//go:build unix

package term_test

import (
	"context"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/blairham/koi-shell/internal/term"
)

// TestTypeAheadReachesTheNextPrompt drives the handover the shell performs
// between every pair of commands: the editor cancels the decoder so the
// command can own stdin, then opens a fresh session for the next prompt.
//
// A session stashes what it read but never delivered, and the next one
// starts from that stash. Those are different goroutines, and nothing
// ordered them, so the outgoing reader could still be mid-read when the
// incoming decode loop had already claimed the stash -- and the bytes it
// stashed a moment later were skipped past the prompt they were typed at.
// The shell then sat with nothing written, which is #279's signature. It
// takes the two goroutines being scheduled in the wrong order, which is
// why it only ever appeared on a loaded CI runner.
//
// Deliberately no drain of the first session: leaving its channel unread
// is what forces the input down the stash path, which is the path under
// test. Repeated, because the failure is a scheduling race.
func TestTypeAheadReachesTheNextPrompt(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	// Neither end is closed: the last session's reader may still be
	// unwinding, and closing the descriptor out from under it is a race in
	// the test rather than in the code under test. The process exit
	// reclaims them.
	tr := term.NewTTY(tty, tty)
	// Raw mode, as the editor does before every read: in canonical mode
	// the line discipline would hold the byte until a newline and no
	// session would see it at all.
	if _, err := tr.EnterRaw(); err != nil {
		t.Skipf("cannot enter raw mode on this pty: %v", err)
	}

	var lastDrained <-chan term.Event

	for run := range 30 {
		// The prompt the user types at, whose command is then accepted.
		ctx, cancel := context.WithCancel(context.Background())
		events, err := tr.Events(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ptmx.WriteString("x"); err != nil {
			t.Fatal(err)
		}
		// Give the reader a chance to pull the byte into user space, so
		// this exercises the stash rather than the kernel buffer.
		time.Sleep(2 * time.Millisecond)
		cancel()

		// The next prompt. The x has to arrive here.
		ctx2, cancel2 := context.WithCancel(context.Background())
		events2, err := tr.Events(ctx2)
		if err != nil {
			t.Fatal(err)
		}
		got := waitForRune(events2, 'x', 2*time.Second)
		cancel2()
		for range events { //nolint:revive // let the first session finish
		}
		for range events2 { //nolint:revive // and the second
		}
		lastDrained = events2
		if !got {
			t.Fatalf("run %d: the x typed as the command was accepted never reached the next prompt", run)
		}
	}
	_ = lastDrained
	_ = tty
}

func waitForRune(events <-chan term.Event, want rune, within time.Duration) bool {
	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return false
			}
			if k, isKey := ev.(term.KeyEvent); isKey && k.Rune == want {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
