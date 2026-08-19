// Copyright (c) 2026, Blair Hamilton. See LICENSE for licensing information.

package interp_test

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"
	"mvdan.cc/sh/v3/syntax"
)

// runViaParseAsRead runs src the way every koi entry point does — through
// [interp.ParseAsRead], which is where a here-document body is put back to
// the literal it was written as (#258). It is deliberately not
// syntax.Parse: the repair needs the source text, and that is the last
// place the source still exists.
func runViaParseAsRead(t *testing.T, src string) string {
	t.Helper()
	stmts, err := interp.ParseAsRead(strings.NewReader(src), "test")
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	var buf bytes.Buffer
	r, err := interp.New(interp.StdIO(strings.NewReader(""), &buf, &buf))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), &syntax.File{Name: "test", Stmts: stmts}); err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	return buf.String()
}

// TestPartlyQuotedHeredocIsLiteral checks POSIX's rule that a body does not
// expand "if any character in word is quoted" — so `<<'E'OF` is as literal
// as `<<'EOF'`, which the parser disagreed with because it reads only the
// last part of the delimiter (#258).
//
// bash is the oracle rather than a hand-written expectation, since the
// question is what bash does and not what we think it ought to.
func TestPartlyQuotedHeredocIsLiteral(t *testing.T) {
	t.Parallel()

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this machine: the differential oracle is unavailable")
	}

	tests := []struct {
		name string
		src  string
	}{{
		name: "the issue: a value and a backslash both survive",
		src:  "V=live\ncat <<'E'OF\nre=\\\\d+ $V\nEOF\n",
	}, {
		// The body is recovered by slicing the source, not by printing
		// the parse tree, and this case is what tells them apart: the
		// printer rewrites a backquote as $( ) and `$((1+1))` with
		// spaces, quietly editing a heredoc whose whole purpose is to
		// hold text exactly.
		name: "a backquote and an arithmetic expansion keep their spelling",
		src:  "cat <<'E'OF\n`echo bq` $((1+1))\nEOF\n",
	}, {
		name: "a double-quoted part counts too",
		src:  "V=live\ncat <<'E'\"O\"F\n$V\nEOF\n",
	}, {
		name: "a backslash-escaped part counts too",
		src:  "V=live\ncat <<\\EO'F'\n$V\nEOF\n",
	}, {
		name: "tab stripping still applies",
		src:  "V=live\ncat <<-'E'OF\n\tre=\\\\d+ $V\n\tEOF\n",
	}, {
		// eval and source re-parse inside the interpreter. They reach
		// ParseAsRead too, which is the gap #288 had to close for the
		// fully quoted form.
		name: "through eval",
		src:  "V=live\neval 'cat <<'\\''E'\\''OF\n$V\nEOF'\n",
	}, {
		name: "the quote last, which already worked",
		src:  "V=live\ncat <<E'OF'\nre=\\\\d+ $V\nEOF\n",
	}, {
		name: "fully quoted, which already worked",
		src:  "V=live\ncat <<'EOF'\nre=\\\\d+ $V\nEOF\n",
	}, {
		// The other half of getting this right: an unquoted delimiter is
		// untouched by any of the above and still expands.
		name: "an unquoted delimiter still expands",
		src:  "V=live\ncat <<EOF\nre=\\\\d+ $V\nEOF\n",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(bashPath)
			cmd.Stdin = strings.NewReader(tc.src)
			want, berr := cmd.CombinedOutput()
			if berr != nil {
				t.Fatalf("bash refused %q: %v (%s)", tc.src, berr, want)
			}
			if got := runViaParseAsRead(t, tc.src); got != string(want) {
				t.Errorf("source %q\n got: %q\nwant: %q (bash)", tc.src, got, want)
			}
		})
	}
}
