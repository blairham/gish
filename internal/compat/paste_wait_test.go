//go:build unix

package compat

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The consume-through rule that #195 turned on.
//
// The gate waits for a command's D mark and then for the next prompt's B
// mark. The shell writes both back to back, so whether they arrive in one
// read or two is decided by whether the reader goroutine happens to be
// scheduled in between — which is to say, by machine load. Clearing the
// buffer between the two waits worked whenever they arrived separately
// and hung for the full idle budget whenever they did not, which is why
// it failed only on loaded CI runners and passed on every rerun.
//
// The two paste cases carrying a Setup step are the only ones that take
// that path, and they were exactly the two that failed.
func TestConsumeThroughKeepsWhatFollowsTheMarker(t *testing.T) {
	t.Parallel()

	const (
		dMark = "\x1b]133;D;0"
		bMark = "\x1b]133;B"
	)
	// One read carrying the finished command *and* the next prompt: the
	// coalescing a loaded machine produces.
	var buf bytes.Buffer
	buf.WriteString("first-command\r\n" + dMark + "\x1b\\" + "\x1b]133;A\x1b\\koi% " + bMark + "\x1b\\")

	idx := bytes.Index(buf.Bytes(), []byte(dMark))
	if idx < 0 {
		t.Fatal("fixture does not contain the D mark")
	}
	consumeThrough(&buf, idx+len(dMark))

	// The prompt mark shared that read and must still be there; before
	// #195 a Reset here discarded it and the next wait blocked forever.
	if !bytes.Contains(buf.Bytes(), []byte(bMark)) {
		t.Fatalf("prompt mark lost with the consumed bytes: %q", buf.String())
	}
	// And what was consumed is gone, so a later scan cannot match the
	// previous command's marks a second time.
	if bytes.Contains(buf.Bytes(), []byte("first-command")) {
		t.Errorf("consumed bytes survived: %q", buf.String())
	}
}

func TestConsumeThroughHandlesTheWholeBuffer(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	buf.WriteString("abc")
	consumeThrough(&buf, buf.Len())
	if buf.Len() != 0 {
		t.Errorf("consuming everything left %q", buf.String())
	}
	// Consuming nothing keeps everything: a marker at offset 0 with an
	// empty match must not clear the buffer.
	buf.WriteString("xyz")
	consumeThrough(&buf, 0)
	if buf.String() != "xyz" {
		t.Errorf("consuming nothing changed the buffer: %q", buf.String())
	}
}

// The failure, reproduced deterministically.
//
// waitPast/waitFor are closures over one buffer inside pasteIntoKoi, so
// this models that pair exactly: wait for the setup command's D mark,
// then for the next prompt's B mark, with both arriving in ONE read.
// That coalescing is what a loaded runner produces, and it is the whole
// difference between the gate passing and hanging for 30s.
func TestSetupWaitSurvivesCoalescedMarks(t *testing.T) {
	t.Parallel()

	const (
		dMark = "\x1b]133;D;"
		bMark = "\x1b]133;B"
	)
	// One read: command output, its D mark, and the next prompt's B mark.
	coalesced := "first-command\r\n" + dMark + "0\x1b\\\x1b]133;A\x1b\\koi% " + bMark + "\x1b\\"

	for _, tt := range []struct {
		name    string
		consume bool // true = waitPast (the fix), false = waitFor + Reset (the bug)
		wantOK  bool
	}{
		{"waitPast keeps the prompt mark", true, true},
		{"reset-between-waits loses it", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var seen bytes.Buffer
			seen.WriteString(coalesced)

			// First wait: the setup command finished.
			if !bytes.Contains(seen.Bytes(), []byte(dMark)) {
				t.Fatal("fixture lacks the D mark")
			}
			if tt.consume {
				consumeThrough(&seen, bytes.Index(seen.Bytes(), []byte(dMark))+len(dMark))
			} else {
				seen.Reset() // what the code used to do here
			}

			// Second wait: is the next prompt's mark still reachable? No
			// more reads are coming — the shell is idle awaiting input.
			gotOK := bytes.Contains(seen.Bytes(), []byte(bMark))
			if gotOK != tt.wantOK {
				t.Errorf("prompt mark reachable = %v, want %v (buffer %q)", gotOK, tt.wantOK, seen.String())
			}
		})
	}
}

// The failure message a stalled paste produces (#279).
//
// One flake has ever been seen, and it cost a wrong diagnosis: the
// message said `no output for 30s waiting for "\x1b]133;D;"; saw ""`,
// which reads as "koi wrote nothing at all" and sent the triage looking
// for a shell that never started. Every part of that reading was
// unsupported by the message — hence these three cases, which pin what
// the next occurrence has to be able to tell someone.
func TestStalledErrorSaysWhichWaitAndWhetherTheShellLives(t *testing.T) {
	readErr := io.EOF

	cases := []struct {
		name        string
		phase       string
		want        string
		total       int
		readErr     *error
		cause       string
		mustContain []string
	}{
		{
			// The setup command and the pasted command both end with a D
			// mark, so the awaited escape named two different moments and
			// the report could not be attributed to either.
			name: "names the phase", phase: "pasted command finishing",
			want: "\x1b]133;D;", total: 412, cause: "nothing written for 30s",
			mustContain: []string{"pasted command finishing", "OSC 133 D"},
		},
		{
			// The buffer is cleared before this wait, so an empty buffer
			// never meant an idle shell. The running total is what
			// separates "wrote nothing ever" from "wrote nothing since".
			name:  "distinguishes no output ever from none since the clear",
			phase: "setup command finishing", want: "\x1b]133;D;", total: 412,
			cause:       "nothing written for 30s",
			mustContain: []string{"412 bytes written in total", "still running"},
		},
		{
			// A dead shell and a quiet one produced the same 30 seconds
			// of silence. The pty read error is the portable way to tell
			// them apart: it fails when the child closes its end.
			name: "says when the shell is gone", phase: "first prompt",
			want: markPromptEnd, total: 0, readErr: &readErr, cause: "the shell exited",
			mustContain: []string{"OSC 133 B", "the shell is gone", "EOF"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen bytes.Buffer
			err := stalledErr(tc.phase, tc.want, &seen, tc.total, tc.readErr, tc.cause)
			for _, want := range tc.mustContain {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q does not mention %q", err, want)
				}
			}
		})
	}
}

// A shell that dies at once must be reported as dead rather than waited
// on: the wait can only ever time out, and 30 seconds of silence is the
// most misleading way to say "the process is not there".
func TestPasteReportsAShellThatDiesImmediately(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash: the oracle is unavailable")
	}
	// `true` stands in for a koi that exits before writing a prompt.
	dead, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) to stand in for a dead shell")
	}

	start := time.Now()
	r := RunPaste(context.Background(), bashBin, dead, PasteCase{
		Name: "dead shell", Text: "echo hi", OracleScript: "echo hi",
	})
	elapsed := time.Since(start)

	if r.Pass {
		t.Fatal("a shell that exits immediately passed the gate")
	}
	// Fast, not pasteIdle: the whole point is that this no longer looks
	// like slowness.
	if elapsed >= pasteIdle {
		t.Errorf("took %s to notice a dead shell; the idle bound is %s", elapsed, pasteIdle)
	}
	if !strings.Contains(r.Reason, "shell is gone") {
		t.Errorf("reason does not say the shell died: %q", r.Reason)
	}
}
