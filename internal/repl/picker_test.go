package repl

import (
	"strings"
	"testing"
	"time"

	"github.com/blairham/koi-shell/internal/history"
)

func TestHistoryDetailShowsWhatMattersForChoosing(t *testing.T) {
	t.Parallel()

	now := time.Now()
	detail := historyDetail(history.Entry{
		Command:       "make test",
		Cwd:           "/work/repo",
		StartedUnixMs: now.Add(-3 * time.Hour).UnixMilli(),
		DurationMs:    4200,
		ExitCode:      2,
	})
	for _, want := range []string{"repo", "3h", "took 4s", "exit 2"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q missing %q", detail, want)
		}
	}

	// A clean, instant, recent command says only where it ran and when.
	quiet := historyDetail(history.Entry{
		Command:       "ls",
		Cwd:           "/tmp",
		StartedUnixMs: now.UnixMilli(),
	})
	if strings.Contains(quiet, "exit") || strings.Contains(quiet, "took") {
		t.Errorf("quiet detail is noisy: %q", quiet)
	}
	if !strings.Contains(quiet, "just now") {
		t.Errorf("quiet detail = %q", quiet)
	}
}

func TestHumanAge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	}
	for _, tt := range tests {
		if got := humanAge(tt.d); got != tt.want {
			t.Errorf("humanAge(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestHistoryPickFnNilWithoutStore(t *testing.T) {
	t.Parallel()

	if historyPickFn(nil, nil) != nil {
		t.Error("a picker was offered with no history to pick from")
	}
}

func TestPickWithoutTerminalDoesNotHang(t *testing.T) {
	// The scripted path: `pick` needs a terminal, and must say so and
	// exit rather than block a pipeline forever.
	rc := t.TempDir() + "/koirc"
	_, errOut, err := runConfigScript(t, rc, "printf 'a\\nb\\n' | pick\n")
	if err == nil {
		t.Error("pick without a terminal should fail, not succeed silently")
	}
	if !strings.Contains(errOut, "terminal") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestPickUsageAndBadFlags(t *testing.T) {
	rc := t.TempDir() + "/koirc"
	out, _, err := runConfigScript(t, rc, "pick --help\n")
	if err != nil || !strings.Contains(out, "usage: … | pick") {
		t.Errorf("help = %q, %v", out, err)
	}
	_, errOut, _ := runConfigScript(t, rc, "printf 'a\\n' | pick --bogus\n")
	if !strings.Contains(errOut, "unknown argument") {
		t.Errorf("stderr = %q", errOut)
	}
}
