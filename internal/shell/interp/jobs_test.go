// Copyright (c) 2026, Blair Hamilton. See LICENSE for licensing information.

package interp_test

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"
	"mvdan.cc/sh/v3/syntax"
)

func runShell(t *testing.T, src string) string {
	t.Helper()
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	r, err := interp.New(interp.StdIO(strings.NewReader(""), &buf, &buf))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), file); err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	return buf.String()
}

// TestJobsBoundsAParallelLoop is the assertion that matters for #302, and it
// is deliberately not "does `jobs` print something".
//
// The bug was that refusing `jobs` left the idiom
//
//	while (( $(jobs -r | wc -l) >= MAX )); do wait -n; done
//
// silently unbounded: `jobs` failed, the count came back zero, the `while`
// never blocked, and a script asking for at most MAX in flight ran all of them
// at once. Nothing errored and the exit status was 0.
//
// The trap this test fell into first is worth stating, because it is the whole
// reason the assertion is shaped this way. Checking only "the reported
// in-flight count never reaches MAX" passes on the *broken* shell too: there
// the count is always 0, which is comfortably under the bound. The measurement
// was being read through the very builtin under test, so a `jobs` that
// answered nothing looked identical to a bound working perfectly. Hence the
// second assertion -- the count must actually climb -- which is what fails
// when `jobs` goes back to refusing.
func TestJobsBoundsAParallelLoop(t *testing.T) {
	t.Parallel()

	const max = 3
	// Each iteration reports how many jobs were already running when it was
	// allowed to start one more. The jobs sleep so that several really are
	// in flight at once; without that both a working and a broken shell
	// would report zero and the test would prove nothing.
	const src = `
		for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
			while [ "$(jobs -r | wc -l)" -ge 3 ]; do wait -n; done
			echo "inflight:$(jobs -r | wc -l)"
			sleep 0.05 &
		done
		wait
		echo done`
	out := runShell(t, src)
	if !strings.Contains(out, "done") {
		t.Fatalf("loop did not finish: %q", out)
	}
	var started, peak int
	for _, line := range strings.Split(out, "\n") {
		n, ok := strings.CutPrefix(line, "inflight:")
		if !ok {
			continue
		}
		started++
		count, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			t.Fatalf("unreadable count %q in %q", n, out)
		}
		if count >= max {
			t.Errorf("%d jobs already running when the loop started another; the bound of %d is not holding (output %q)",
				count, max, out)
		}
		peak = max2(peak, count)
	}
	if started != 12 {
		t.Errorf("expected 12 iterations, saw %d: %q", started, out)
	}
	// The half that catches a `jobs` which answers nothing at all: if the
	// count never rises above zero while twelve sleeping jobs are being
	// started three at a time, the loop is not being bounded, it is being
	// lied to.
	if peak == 0 {
		t.Errorf("jobs -r never reported a running job across %d iterations, so the bound is absent rather than held: %q",
			started, out)
	}
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestJobsIsVisibleWhereBashMakesItVisible pins the scope rule, which is the
// part that makes or breaks the loop above. koi's subshells deliberately do
// not inherit the jobs they can wait for -- but bash still *reports* the
// caller's jobs inside a command substitution and inside a pipeline stage, and
// those two are exactly what `$(jobs -r | wc -l)` is made of. An explicit
// `( ... )` subshell reports none, in bash and here alike.
func TestJobsIsVisibleWhereBashMakesItVisible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want string
	}{
		{"directly", `sleep 1 & jobs -r | wc -l`, "1"},
		{"through a pipeline stage", `sleep 1 & jobs -r | cat | wc -l`, "1"},
		{"inside a command substitution", `sleep 1 & echo "$(jobs -r | wc -l)"`, "1"},
		{"not inside an explicit subshell", `sleep 1 & ( jobs -r ) | wc -l`, "0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := strings.TrimSpace(runShell(t, tc.src))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestJobsListingMatchesBash pins the format, measured against bash 5.3 rather
// than invented: "[N]M" then two spaces, the status in a 27-column field, then
// the command -- which a running job carries a trailing " &" on and a finished
// one does not.
func TestJobsListingMatchesBash(t *testing.T) {
	t.Parallel()

	got := runShell(t, `sleep 1 & sleep 1 & jobs`)
	want := "[1]-  Running                    sleep 1 &\n" +
		"[2]+  Running                    sleep 1 &\n"
	if got != want {
		t.Errorf("jobs listing\n got %q\nwant %q", got, want)
	}
}

// TestJobsReportsACompletedJobOnce follows bash, which mentions a job's
// completion and then drops it from the table.
func TestJobsReportsACompletedJobOnce(t *testing.T) {
	t.Parallel()

	got := runShell(t, `{ read -r _ < /dev/null; } & wait; jobs; echo ---; jobs`)
	if !strings.Contains(got, "Done") {
		t.Fatalf("the finished job was never reported: %q", got)
	}
	if after := got[strings.Index(got, "---"):]; strings.Contains(after, "Done") {
		t.Errorf("the same completion was reported twice: %q", got)
	}
}

// TestWaitRefusesAnInheritedJob is the other half of the visibility rule: a
// command substitution can see the caller's jobs but is not their parent, and
// bash answers "not a child of this shell" when it tries to wait on one.
func TestWaitRefusesAnInheritedJob(t *testing.T) {
	t.Parallel()

	got := runShell(t, `sleep 1 & p=$!; echo "$(wait "$p" 2>&1)"`)
	if !strings.Contains(got, "not a child of this shell") {
		t.Errorf("a command substitution was allowed to wait on the caller's job: %q", got)
	}
}

// TestBuiltinListsAgree is the #302 headline: `type`, `compgen -b` and running
// the builtin all have to give the same answer about whether it exists.
func TestBuiltinListsAgree(t *testing.T) {
	t.Parallel()

	if got := runShell(t, `type jobs`); !strings.Contains(got, "shell builtin") {
		t.Errorf("type jobs: %q", got)
	}
	listed := runShell(t, `compgen -b`)
	if !slicesContainsLine(listed, "jobs") {
		t.Errorf("compgen -b does not list jobs: %q", listed)
	}
	// Every name compgen -b advertises has to actually run, which is the
	// property that was broken rather than any single name.
	for _, name := range interp.ImplementedBuiltins() {
		if !slicesContainsLine(listed, name) {
			t.Errorf("compgen -b omits implemented builtin %q", name)
		}
	}
	for _, name := range interp.UnimplementedBuiltins() {
		if slicesContainsLine(listed, name) {
			t.Errorf("compgen -b advertises %q, which does not run", name)
		}
	}
}

func slicesContainsLine(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return true
		}
	}
	return false
}
