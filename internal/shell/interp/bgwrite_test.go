// Copyright (c) 2026, Blair Hamilton. See LICENSE for licensing information.

package interp_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"
	"github.com/blairham/koi-shell/internal/shell/syntax"
)

// TestBackgroundJobsDoNotShareALine checks that concurrent background jobs each
// land their output whole.
//
// The bug this guards (#301) has two faces, and only the second one is loud.
// Against a terminal or a pipe, a builtin that wrote once per argument let
// another job's write land mid-line, so "done:2" and "done:3" arrived fused as
// "done:2done:3" -- wrong output, exit status 0, nothing on stderr. Against any
// other writer it is worse: the concurrent writes are a plain data race, and a
// bytes.Buffer answers one by discarding the other, so lines go missing
// outright. This test uses a buffer because that catches both -- a fused line
// fails the format check and a dropped one fails the count -- and because it
// puts the race under -race, where it is a hard failure rather than a
// probability.
//
// Repeated because the failure is timing-dependent: a single run passed often
// enough on the original code to look fine.
func TestBackgroundJobsDoNotShareALine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want []string
	}{{
		name: "echo",
		src:  `for i in 1 2 3 4 5 6 7 8; do { echo "done:$i"; } & done; wait`,
		want: []string{"done:1", "done:2", "done:3", "done:4", "done:5", "done:6", "done:7", "done:8"},
	}, {
		// Several arguments was the original shape: one write per
		// argument plus one for the newline meant three chances to be
		// interrupted in a single echo.
		name: "echo with several arguments",
		src:  `for i in 1 2 3 4; do { echo alpha beta "gamma:$i"; } & done; wait`,
		want: []string{"alpha beta gamma:1", "alpha beta gamma:2", "alpha beta gamma:3", "alpha beta gamma:4"},
	}, {
		// printf recycling its format over the argument list is one
		// write per cycle, so it has the same exposure as echo.
		name: "printf recycling its format",
		src:  `for i in 1 2; do { printf "p:%s\n" a b c; } & done; wait`,
		want: []string{"p:a", "p:a", "p:b", "p:b", "p:c", "p:c"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file, err := syntax.NewParser().Parse(strings.NewReader(tc.src), "")
			if err != nil {
				t.Fatal(err)
			}
			for run := range 50 {
				var buf bytes.Buffer
				r, err := interp.New(interp.StdIO(nil, &buf, &buf))
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Run(context.Background(), file); err != nil {
					t.Fatalf("run %d: %v", run, err)
				}
				// Sorted, because the jobs finish in whatever
				// order they finish in -- the ordering is not
				// what is under test, the integrity of each
				// line is.
				got := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
				if len(got) != len(tc.want) {
					t.Fatalf("run %d: got %d lines, want %d: %q",
						run, len(got), len(tc.want), buf.String())
				}
				slices.Sort(got)
				for i, w := range tc.want {
					if got[i] != w {
						t.Fatalf("run %d: line %d is %q, want %q (whole output %q)",
							run, i, got[i], w, buf.String())
					}
				}
			}
		})
	}
}

// TestPipelineStagesDoNotShareALine covers the other way koi runs two writers
// at once. A pipeline's stages are concurrent goroutines sharing stderr, and
// unlike a background job nothing in the source says so -- there is no "&" to
// warn a reader that two things are about to write one destination.
func TestPipelineStagesDoNotShareALine(t *testing.T) {
	t.Parallel()

	src := `{ echo left >&2; } | { echo right >&2; }`
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatal(err)
	}
	for run := range 50 {
		var buf bytes.Buffer
		r, err := interp.New(interp.StdIO(nil, &buf, &buf))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Run(context.Background(), file); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		got := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		slices.Sort(got)
		if len(got) != 2 || got[0] != "left" || got[1] != "right" {
			t.Fatalf("run %d: got %q, want left and right intact", run, buf.String())
		}
	}
}
