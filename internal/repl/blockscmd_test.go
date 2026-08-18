package repl

import (
	"strings"
	"testing"
	"time"

	"github.com/blairham/koi-shell/internal/history"
)

// blockIndex is the parse between what a user reads off the listing and
// which entry that is. Off-by-one here shows someone the wrong output,
// which is worse than an error.
func TestBlockIndexIsOneBased(t *testing.T) {
	if _, err := blockIndex("1", 3); err != nil {
		t.Errorf("index 1 rejected: %v", err)
	}
	got, err := blockIndex("3", 3)
	if err != nil || got != 2 {
		t.Errorf("index 3 of 3 = %d, %v; want 2, nil", got, err)
	}
	for _, bad := range []string{"0", "4", "-1", "x", ""} {
		if _, err := blockIndex(bad, 3); err == nil {
			t.Errorf("index %q was accepted", bad)
		}
	}
	// With nothing captured, the error should point at the switch rather
	// than at the number.
	_, err = blockIndex("1", 0)
	if err == nil || !strings.Contains(err.Error(), "config blocks on") {
		t.Errorf("empty-store error does not name the fix: %v", err)
	}
}

// A search hit shows the line that matched, so the result is
// informative rather than just a command name.
func TestFirstMatchingLine(t *testing.T) {
	out := "compiling\nerror: missing symbol\nlinking\n"
	if got := firstMatchingLine(out, "error"); got != "error: missing symbol" {
		t.Errorf("got %q", got)
	}
	if got := firstMatchingLine(out, "absent"); got != "" {
		t.Errorf("no match should yield empty, got %q", got)
	}
	// Carriage returns come with pty capture and must not leak into the
	// rendered line.
	if got := firstMatchingLine("a\r\nerror here\r\n", "error"); got != "error here" {
		t.Errorf("CR not trimmed: %q", got)
	}
}

// The detail column beside a command in the picker: when it ran, how it
// exited, where it ran. Enough to choose between two runs of the same
// command without opening either.
func TestBlockDetail(t *testing.T) {
	now := time.Now()
	e := history.Entry{
		Command:       "make build",
		StartedUnixMs: now.Add(-90 * time.Minute).UnixMilli(),
		ExitCode:      2,
		Cwd:           "/tmp/project",
	}
	got := blockDetail(e, now)
	for _, want := range []string{"1h", "exit 2", "project"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail %q missing %q", got, want)
		}
	}

	// A clean command says nothing about exit status — a column that
	// always reads "exit 0" is noise.
	ok := history.Entry{Command: "ls", StartedUnixMs: now.UnixMilli()}
	if strings.Contains(blockDetail(ok, now), "exit") {
		t.Errorf("clean command mentions exit: %q", blockDetail(ok, now))
	}
}
