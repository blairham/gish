//go:build unix

package jobs_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/jobs"
)

// outFile is a real file for the runner's stdout/stderr: the job
// middleware only manages processes whose stdio are files (matching the
// interactive terminal), so tests must use files, not buffers.
func outFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func readBack(t *testing.T, f *os.File) string {
	t.Helper()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// newRunner builds a runner with job control middleware and no tty
// (terminal handoff skipped; grouping and stop tracking still active).
func newRunner(t *testing.T, table *jobs.Table, out *os.File) *interp.Runner {
	t.Helper()
	runner, err := interp.New(
		interp.StdIO(nil, out, out),
		interp.ExecHandlers(table.ExecMiddleware),
		interp.CallHandler(jobs.RewriteCall),
	)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func runLine(t *testing.T, table *jobs.Table, runner *interp.Runner, line string) (jobs.Notice, bool, error) {
	t.Helper()
	file, err := syntax.NewParser().Parse(strings.NewReader(line), "test")
	if err != nil {
		t.Fatal(err)
	}
	table.BeginLine(line)
	rerr := runner.Run(context.Background(), file)
	n, ok := table.EndLine()
	return n, ok, rerr
}

func hcFor(out io.Writer) interp.HandlerContext {
	// Minimal handler context for calling the builtins directly.
	return interp.HandlerContext{Stdout: out, Stderr: out}
}

func TestForegroundCompletesAndIsNotFiled(t *testing.T) {
	t.Parallel()

	table := jobs.NewTable(nil)
	out := outFile(t)
	runner := newRunner(t, table, out)
	_, filed, err := runLine(t, table, runner, "echo grouped")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if filed {
		t.Error("completed foreground line was filed as a job")
	}
	if !strings.Contains(readBack(t, out), "grouped") {
		t.Errorf("output = %q", readBack(t, out))
	}
}

func TestSelfStoppingJobIsFiledAndBgResumes(t *testing.T) {
	t.Parallel()

	table := jobs.NewTable(nil)
	out := outFile(t)
	runner := newRunner(t, table, out)

	// The child stops itself: the handler must observe the stop, the
	// line must end with the job filed as Stopped.
	n, filed, _ := runLine(t, table, runner, `sh -c 'kill -STOP $$; exit 3'`)
	if !filed || !n.Stopped {
		t.Fatalf("stop not filed: notice=%+v filed=%v", n, filed)
	}

	// jobs lists it.
	var list strings.Builder
	if err := table.Jobs(context.Background(), hcFor(&list), nil); err != nil {
		t.Fatalf("jobs: %v", err)
	}
	if !strings.Contains(list.String(), "Stopped") {
		t.Errorf("jobs output = %q", list.String())
	}

	// bg resumes it; the reaper collects the exit and empties the table.
	var bgOut strings.Builder
	if err := table.Bg(context.Background(), hcFor(&bgOut), nil); err != nil {
		t.Fatalf("bg: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var l strings.Builder
		_ = table.Jobs(context.Background(), hcFor(&l), nil)
		if l.Len() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never reaped after bg: %q", l.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFgWaitsAndReturnsExitStatus(t *testing.T) {
	t.Parallel()

	table := jobs.NewTable(nil)
	out := outFile(t)
	runner := newRunner(t, table, out)

	if _, filed, _ := runLine(t, table, runner, `sh -c 'kill -STOP $$; exit 7'`); !filed {
		t.Fatal("stop not filed")
	}
	var fgOut strings.Builder
	err := table.Fg(context.Background(), hcFor(&fgOut), nil)
	status, ok := errors.AsType[interp.ExitStatus](err)
	if !ok || int(status) != 7 {
		t.Fatalf("fg err = %v, want exit status 7", err)
	}

	// Table must be empty afterwards.
	var l strings.Builder
	_ = table.Jobs(context.Background(), hcFor(&l), nil)
	if l.Len() != 0 {
		t.Errorf("jobs after fg = %q", l.String())
	}
}

func TestFgWithNoJobs(t *testing.T) {
	t.Parallel()

	table := jobs.NewTable(nil)
	var out strings.Builder
	err := table.Fg(context.Background(), interp.HandlerContext{Stdout: &out, Stderr: &out}, nil)
	status, ok := errors.AsType[interp.ExitStatus](err)
	if !ok || int(status) != 1 {
		t.Fatalf("fg err = %v, want exit status 1", err)
	}
	if !strings.Contains(out.String(), "no current job") {
		t.Errorf("output = %q", out.String())
	}
}

func TestPipelineSharesOneProcessGroup(t *testing.T) {
	t.Parallel()

	table := jobs.NewTable(nil)
	out := outFile(t)
	runner := newRunner(t, table, out)

	// Both pipeline stages report their own process group id; job
	// control must place them in the same group.
	_, _, err := runLine(t, table, runner, `sh -c 'ps -o pgid= -p $$' | sh -c 'ps -o pgid= -p $$; cat'`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	fields := strings.Fields(readBack(t, out))
	if len(fields) != 2 {
		t.Fatalf("expected two pgids, got %q", fields)
	}
	if fields[0] != fields[1] {
		t.Errorf("pipeline stages in different process groups: %v", fields)
	}
}

func TestRewriteCall(t *testing.T) {
	t.Parallel()

	args, err := jobs.RewriteCall(context.Background(), []string{"jobs"})
	if err != nil || args[0] != "__koi_jobs" {
		t.Errorf("rewrite = %v, %v", args, err)
	}
	args, _ = jobs.RewriteCall(context.Background(), []string{"echo", "fg"})
	if args[0] != "echo" || args[1] != "fg" {
		t.Errorf("non-builtin rewritten: %v", args)
	}
}
