//go:build unix

package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blairham/koi-shell/internal/acp"
)

// The kill contract (#328, #329): stopping a terminal stops the whole
// command tree, and waiting on a stopped terminal returns. Both were
// false — Kill signaled one pid, so under a sandbox profile it stopped
// only the `koi __sandbox-exec` wrapper, and Wait then blocked forever
// on the output pipe the orphans still held.

// startPidWriter runs a shell line that backgrounds a long sleep,
// writes the grandchild's pid to a file (spelled PIDFILE in the
// script), then holds. It returns the process handle and the
// grandchild's pid once it is known.
func startPidWriter(t *testing.T, shell, script string) (acp.Process, int) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	out := acp.NewOutput(0)
	proc, err := acp.ExecRunner(context.Background(), acp.Command{
		Command: shell,
		Args:    []string{"-c", strings.ReplaceAll(script, "PIDFILE", pidFile)},
		Cwd:     dir,
	}, out)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if raw, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(raw)) != "" {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatalf("pid file held %q: %v", raw, err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return proc, pid
		}
		if time.Now().After(deadline) {
			_ = proc.Kill()
			t.Fatal("the command never wrote its grandchild's pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitGone polls until the pid stops existing. A freshly orphaned
// process is reaped by init, so ESRCH arrives shortly after the kill —
// or never, which is the bug.
func waitGone(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d survived the kill — the command's descendants were orphaned, not stopped", pid)
}

// Kill must reach the command's descendants, not just the direct child
// (#328). The grandchild shares the group because ExecRunner started
// the command with Setpgid — exactly what the sandbox wrapper chain
// looks like in production.
func TestKillReachesDescendants(t *testing.T) {
	t.Parallel()

	proc, grandchild := startPidWriter(t, "/bin/sh",
		`sleep 300 & echo $! > "PIDFILE"; sleep 300`)
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	waitGone(t, grandchild, 5*time.Second)

	// And Wait returns promptly once the tree is dead (#329's common
	// case): nothing holds the output pipe anymore.
	done := make(chan string, 1)
	go func() {
		_, sig, _ := proc.Wait()
		done <- sig
	}()
	select {
	case sig := <-done:
		if sig != "killed" {
			t.Errorf("signal = %q, want %q", sig, "killed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the whole tree was killed")
	}
}

// Wait must return even when a descendant escapes the group kill and
// keeps the output pipe open (#329's backstop). `set -m` gives the
// background job its own process group — the double-forked-daemon
// shape — so only WaitDelay can end the wait. bash rather than /bin/sh
// because dash's job control is not guaranteed non-interactively.
func TestWaitReturnsWhenADescendantHoldsThePipe(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this machine")
	}
	acp.SetWaitDelay(t, 500*time.Millisecond)

	for _, tc := range []struct {
		name     string
		exit     string
		wantCode int
	}{
		// exit 0 is the case where Go reports ErrWaitDelay instead of
		// nil; reporting that as a failure would tell the agent a
		// successful command failed.
		{"clean exit", "exit 0", 0},
		{"failing exit", "exit 7", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc, holder := startPidWriter(t, bash,
				`set -m; sleep 300 & echo $! > "PIDFILE"; `+tc.exit)
			done := make(chan struct{})
			var code int
			var sig string
			var werr error
			go func() {
				code, sig, werr = proc.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Wait hung on the pipe a surviving descendant holds — the #329 deadlock")
			}
			if werr != nil || code != tc.wantCode || sig != "" {
				t.Errorf("Wait = (%d, %q, %v), want (%d, \"\", nil)", code, sig, werr, tc.wantCode)
			}
			_ = syscall.Kill(holder, syscall.SIGKILL)
		})
	}
}

// The protocol-level pin: ACP tells agents to race a timer against
// wait_for_exit and then call kill — and that exact sequence froze the
// session when the command had children (#329).
func TestWaitForExitReturnsAfterKill(t *testing.T) {
	t.Parallel()

	terminals := acp.NewTerminals(acp.ExecRunner)
	ctx := context.Background()
	params, _ := json.Marshal(map[string]any{
		"sessionId": "sess",
		"command":   "/bin/sh",
		"args":      []string{"-c", "(sleep 300 &) ; sleep 300"},
		"cwd":       t.TempDir(),
	})
	created, err := terminals.Handle(ctx, "terminal/create", params)
	if err != nil {
		t.Fatal(err)
	}
	termID := created.(map[string]any)["terminalId"].(string)
	t.Cleanup(terminals.ReleaseAll)
	ref, _ := json.Marshal(map[string]string{"sessionId": "sess", "terminalId": termID})

	// Let the shell finish forking; killing mid-fork passes vacuously,
	// which is how the live repro's first run "passed".
	time.Sleep(300 * time.Millisecond)
	if _, err := terminals.Handle(ctx, "terminal/kill", ref); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := terminals.Handle(ctx, "terminal/wait_for_exit", ref)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("wait_for_exit after kill: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("terminal/wait_for_exit never returned after terminal/kill — the session is deadlocked")
	}
}
