package repl

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The handshake deadline (#330): `koi acp -- <something mute>` — the
// wrong binary, a program that is not an ACP agent at all — used to
// block on initialize forever, with Ctrl-C as the only way out.
func TestACPHandshakeTimesOutOnAMuteAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sleep as the mute agent")
	}
	old := handshakeTimeout
	handshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	var out, errOut bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- RunACP(context.Background(), strings.NewReader(""), &out, &errOut,
			[]string{"--profile", "none", "--", "/bin/sleep", "60"})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a mute agent completed the handshake")
		}
		// The error must say what happened in the session's terms — a
		// bare context.DeadlineExceeded reads as koi timing out, when
		// what happened is the agent never spoke.
		if !strings.Contains(err.Error(), "did not answer the ACP handshake") {
			t.Errorf("the timeout is not explained: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunACP hung on a mute agent — the handshake has no deadline")
	}
}

// And the deadline must not break a healthy agent: the handshake
// completes, the banner prints, and EOF on stdin ends the session
// cleanly.
func TestACPHandshakeSucceedsWithinDeadline(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fakeagent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin,
		"github.com/blairham/koi-shell/internal/acp/testdata/fakeagent")
	if msg, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the fake agent: %v\n%s", err, msg)
	}

	var out, errOut bytes.Buffer
	err := RunACP(context.Background(), strings.NewReader(""), &out, &errOut,
		[]string{"--profile", "none", "--", bin})
	if err != nil {
		t.Fatalf("RunACP against a healthy agent: %v", err)
	}
	if !strings.Contains(errOut.String(), "koi: hosting") {
		t.Errorf("no hosting banner, so the handshake did not complete: %q", errOut.String())
	}
}
