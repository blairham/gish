// Copyright (c) 2026, Blair Hamilton. See LICENSE for licensing information.

package interp_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"
	"mvdan.cc/sh/v3/syntax"
)

// TestWaitAnyOutlivesItsCall covers the shape `wait -n` was added for: wait for
// one of several jobs, start more, repeat.
//
// The watchers `wait -n` leaves behind are the point. The one whose job
// finishes first answers and the call returns; the rest stay blocked until
// their own jobs end, which is after the shell has moved on and started
// appending to the job slice. A watcher that reached back into that slice was
// racing the append (#313), so the first group here is deliberately slow --
// its watchers are still parked while the later groups reallocate underneath
// them.
//
// It asserts nothing beyond completing, because the failure is a data race:
// the -race detector is the assertion, and without it this test is not
// meaningful.
func TestWaitAnyOutlivesItsCall(t *testing.T) {
	t.Parallel()

	const src = `
		sleep 0.2 & sleep 0.2 & sleep 0.2 & wait -n
		sleep 0.01 & sleep 0.01 & sleep 0.01 & sleep 0.01 & wait -n
		sleep 0.01 & sleep 0.01 & sleep 0.01 & sleep 0.01 & wait -n
		wait
		echo done`
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatal(err)
	}
	for run := range 5 {
		var buf bytes.Buffer
		r, err := interp.New(interp.StdIO(strings.NewReader(""), &buf, &buf))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Run(context.Background(), file); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if !strings.Contains(buf.String(), "done") {
			t.Fatalf("run %d did not finish: %q", run, buf.String())
		}
	}
}
