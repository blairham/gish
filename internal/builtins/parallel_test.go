package builtins

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// runShell runs src through a runner with the builtins ExecHandler, the
// same seam the shell uses, and returns stdout+stderr and the error.
func runShell(t *testing.T, src, stdin string) (string, error) {
	t.Helper()
	var out strings.Builder
	var in *strings.Reader
	if stdin != "" {
		in = strings.NewReader(stdin)
	} else {
		in = strings.NewReader("")
	}
	runner, err := interp.New(
		interp.StdIO(in, &out, &out),
		interp.ExecHandlers(ExecHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "test")
	if err != nil {
		t.Fatal(err)
	}
	rerr := runner.Run(context.Background(), file)
	return out.String(), rerr
}

func TestParallelStreamsWithPrefix(t *testing.T) {
	t.Parallel()

	out, err := runShell(t, `parallel -j 2 -- echo hi {} ::: a b c`, "")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	sort.Strings(lines)
	want := []string{"[a] hi a", "[b] hi b", "[c] hi c"}
	if !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}

func TestParallelAppendsWithoutPlaceholder(t *testing.T) {
	t.Parallel()

	out, err := runShell(t, `parallel -j 1 -- echo ::: x`, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != "[x] x" {
		t.Errorf("out = %q", got)
	}
}

func TestParallelCollectKeepsInputOrder(t *testing.T) {
	t.Parallel()

	// The slowest task comes first: collect must still print in input
	// order, proving buffering rather than completion order.
	out, err := runShell(t,
		`parallel -j 3 --collect -- sh -c 'sleep 0.{}; echo done {}' ::: 3 1 0`, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "done 3\ndone 1\ndone 0\n"
	if out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestParallelWorstExitStatus(t *testing.T) {
	t.Parallel()

	_, err := runShell(t, `parallel -j 2 -- sh -c 'exit {}' ::: 0 3 1`, "")
	status, ok := errors.AsType[interp.ExitStatus](err)
	if !ok || int(status) != 3 {
		t.Errorf("err = %v, want exit status 3", err)
	}
}

func TestParallelStdinFed(t *testing.T) {
	t.Parallel()

	out, err := runShell(t, `parallel -j 2 -- echo got {}`, "one\ntwo\n\n")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	sort.Strings(lines)
	want := []string{"[one] got one", "[two] got two"}
	if !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}

func TestParallelFailFastCancels(t *testing.T) {
	t.Parallel()

	// One worker: task 1 fails immediately; the queued sleep must be
	// skipped, so the whole run finishes far under the sleep duration.
	out, err := runShell(t,
		`parallel -j 1 --fail-fast --collect -- sh -c 'if [ {} = boom ]; then exit 9; fi; sleep 5; echo survived' ::: boom slow`, "")
	if _, ok := errors.AsType[interp.ExitStatus](err); !ok {
		t.Errorf("err = %v, want nonzero exit", err)
	}
	if strings.Contains(out, "survived") {
		t.Error("queued task ran despite --fail-fast")
	}
}

func TestParallelUsageErrors(t *testing.T) {
	t.Parallel()

	for _, src := range []string{
		`parallel ::: a b`,               // no command
		`parallel -j nope -- echo ::: a`, // bad -j
	} {
		out, err := runShell(t, src, "")
		status, ok := errors.AsType[interp.ExitStatus](err)
		if !ok || int(status) != 2 {
			t.Errorf("%q: err = %v, want exit 2", src, err)
		}
		if !strings.Contains(out, "usage: parallel") {
			t.Errorf("%q: no usage in output: %q", src, out)
		}
	}
}

func TestParallelMissingCommand(t *testing.T) {
	t.Parallel()

	out, err := runShell(t, `parallel -j 2 -- definitely-not-a-command-xyz ::: a`, "")
	status, ok := errors.AsType[interp.ExitStatus](err)
	if !ok || int(status) != 127 {
		t.Errorf("err = %v, want exit 127", err)
	}
	if !strings.Contains(out, "parallel: a:") {
		t.Errorf("no start-failure message: %q", out)
	}
}

func TestTaskArgv(t *testing.T) {
	t.Parallel()

	got := taskArgv([]string{"cp", "{}", "{}.bak"}, "f.txt")
	if !slices.Equal(got, []string{"cp", "f.txt", "f.txt.bak"}) {
		t.Errorf("substituted argv = %q", got)
	}
	got = taskArgv([]string{"gzip", "-9"}, "log.txt")
	if !slices.Equal(got, []string{"gzip", "-9", "log.txt"}) {
		t.Errorf("appended argv = %q", got)
	}
}
