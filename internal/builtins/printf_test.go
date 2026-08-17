package builtins

import (
	"io"
	"strings"
	"testing"
)

// Unit coverage for the parts of printf that a differential test cannot
// reach quickly, plus the benchmark.
//
// The bash-differential suite (cmd/gish/printf_test.go) is the oracle
// for *what* the output should be. These are here for the cases that are
// awkward to spell in a shell script, and because a formatting bug found
// in 40µs is cheaper than one found by launching two shells.

func TestPrintfReusesFormat(t *testing.T) {
	t.Parallel()

	var sb strings.Builder
	if err := Printf(&sb, []string{"%s-%s\n", "a", "b", "c", "d"}); err != nil {
		t.Fatal(err)
	}
	if got, want := sb.String(), "a-b\nc-d\n"; got != want {
		t.Errorf("format reuse = %q, want %q", got, want)
	}
}

// A format whose arguments run out mid-way fills the rest with empties
// and stops — it must not loop, and it must not panic reslicing past the
// end, which is how the first version of this failed.
func TestPrintfRunsOutOfArguments(t *testing.T) {
	t.Parallel()

	var sb strings.Builder
	if err := Printf(&sb, []string{"[%s][%d]\n", "onlyone"}); err != nil {
		t.Fatal(err)
	}
	if got, want := sb.String(), "[onlyone][0]\n"; got != want {
		t.Errorf("short argument list = %q, want %q", got, want)
	}
}

// A format with no conversions must print once, not once per argument.
func TestPrintfWithoutConversionsPrintsOnce(t *testing.T) {
	t.Parallel()

	var sb strings.Builder
	if err := Printf(&sb, []string{"fixed\n", "ignored", "also"}); err != nil {
		t.Fatal(err)
	}
	if got, want := sb.String(), "fixed\n"; got != want {
		t.Errorf("constant format = %q, want %q", got, want)
	}
}

// \c stops all output, including text already queued after it and any
// remaining passes over the format.
func TestPrintfStopsAtBackslashC(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"in the format", "keep", []string{`keep\cdropped`}},
		{"in a %b argument", "keep", []string{"%b", `keep\cdropped`}},
	} {
		var sb strings.Builder
		if err := Printf(&sb, tc.args); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if sb.String() != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, sb.String(), tc.want)
		}
	}
}

func TestShellQuoteRoundTrips(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"", "''"},
		{"a b", `a\ b`},
		{"it's", `it\'s`},
		{"a$b", `a\$b`},
		{"new\nline", `$'new\nline'`},
		{"tab\there", `$'tab\there'`},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Performance (#55). printf runs inside loops and prompt helpers, so the
// cost that matters is per call, not per process.
//
// These are benchmarks rather than asserted budgets on purpose: a
// wall-clock threshold on a shared CI runner fails honest code, which is
// docs/bench.md's standing rule. The number is here to be compared
// against itself over time via `go test -bench`.
func BenchmarkPrintfSimple(b *testing.B) {
	args := []string{"%s\n", "hello"}
	b.ReportAllocs()
	for b.Loop() {
		if err := Printf(io.Discard, args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPrintfFloatPrecision(b *testing.B) {
	args := []string{"%08.3f|%+d|%x\n", "3.14159", "42", "255"}
	b.ReportAllocs()
	for b.Loop() {
		if err := Printf(io.Discard, args); err != nil {
			b.Fatal(err)
		}
	}
}

// The reuse path walks the format once per argument group; a regression
// here would show up in scripts that print tables.
func BenchmarkPrintfFormatReuse(b *testing.B) {
	args := []string{"%s\t%s\n"}
	for range 50 {
		args = append(args, "col-a", "col-b")
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := Printf(io.Discard, args); err != nil {
			b.Fatal(err)
		}
	}
}
