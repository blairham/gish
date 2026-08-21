package builtins

import (
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

// Unit coverage for the parts of printf that a differential test cannot
// reach quickly, plus the benchmark.
//
// The bash-differential suite (cmd/koi/printf_test.go) is the oracle
// for *what* the output should be. These are here for the cases that are
// awkward to spell in a shell script, and because a formatting bug found
// in 40µs is cheaper than one found by launching two shells.

func TestPrintfReusesFormat(t *testing.T) {
	t.Parallel()

	var sb strings.Builder
	if err := Printf(&sb, io.Discard, "", []string{"%s-%s\n", "a", "b", "c", "d"}); err != nil {
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
	if err := Printf(&sb, io.Discard, "", []string{"[%s][%d]\n", "onlyone"}); err != nil {
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
	if err := Printf(&sb, io.Discard, "", []string{"fixed\n", "ignored", "also"}); err != nil {
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
		if err := Printf(&sb, io.Discard, "", tc.args); err != nil {
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
		if err := Printf(io.Discard, io.Discard, "", args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPrintfFloatPrecision(b *testing.B) {
	args := []string{"%08.3f|%+d|%x\n", "3.14159", "42", "255"}
	b.ReportAllocs()
	for b.Loop() {
		if err := Printf(io.Discard, io.Discard, "", args); err != nil {
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
		if err := Printf(io.Discard, io.Discard, "", args); err != nil {
			b.Fatal(err)
		}
	}
}

// The numeric grammar (#222), as units.
//
// These matter more than the usual unit test because the bash oracle
// for them is gated: bash 3.2 — a stock macOS /bin/bash — reports these
// cases differently, so on that platform the differential skips and
// this is the only thing checking the rules.
//
// They are written out rather than delegated to strconv.ParseInt with
// base 0 because Go's base-0 grammar is Go's: it accepts 0b101 and
// 1_000, which C does not. Getting that wrong would not fail loudly —
// it would compute a different number than bash for the same script.
func TestParseIntFollowsC(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr string // "" for none, else the Reason
	}{
		{"42", 42, ""},
		{" 42", 42, ""}, // leading whitespace is skipped
		{"+5", 5, ""},
		{"-5", -5, ""},
		{"0", 0, ""},
		{"010", 8, ""},               // a leading zero is octal
		{"0x10", 16, ""},             // and 0x is hex
		{"0X1f", 31, ""},             // either case
		{"'a", 97, ""},               // POSIX character value
		{`"a`, 97, ""},               // either quote
		{"'ab", 97, ""},              // the rest is ignored, without complaint
		{"'", 0, ""},                 // a lone quote is zero
		{"42 ", 42, reasonInvalid},   // trailing space is trailing junk
		{"12abc", 12, reasonInvalid}, // the value read still counts
		{"1_000", 1, reasonInvalid},  // Go would say 1000
		{"0b101", 0, reasonInvalid},  // Go would say 5
		{"", 0, reasonInvalid},
		{" ", 0, reasonInvalid},
		{"notanum", 0, reasonInvalid},
		{"99999999999999999999999", math.MaxInt64, reasonRange},
		{"-99999999999999999999", math.MinInt64, reasonRange},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseInt(tc.in)
			if got != tc.want {
				t.Errorf("parseInt(%q) = %d, want %d", tc.in, got, tc.want)
			}
			assertNumberError(t, tc.in, err, tc.wantErr)
		})
	}
}

func TestParseFloatFollowsC(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in      string
		want    float64
		wantErr string
	}{
		{"3.5", 3.5, ""},
		{" 3.5", 3.5, ""},
		{"-2", -2, ""},
		{"'a", 97, ""}, // the character-value form applies here too
		{"3.5x", 3.5, reasonInvalid},
		{"", 0, reasonInvalid},
		{"notanum", 0, reasonInvalid},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseFloat(tc.in)
			if got != tc.want {
				t.Errorf("parseFloat(%q) = %v, want %v", tc.in, got, tc.want)
			}
			assertNumberError(t, tc.in, err, tc.wantErr)
		})
	}
}

// An overflowing float is an infinity *and* a complaint, which is the
// case that made the C spelling of infinity matter.
func TestParseFloatOverflows(t *testing.T) {
	t.Parallel()
	got, err := parseFloat("1e400")
	if !math.IsInf(got, 1) {
		t.Errorf("parseFloat(1e400) = %v, want +Inf", got)
	}
	assertNumberError(t, "1e400", err, reasonRange)
}

func assertNumberError(t *testing.T, in string, err error, wantReason string) {
	t.Helper()
	if wantReason == "" {
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
		}
		return
	}
	var ne *NumberError
	if !errors.As(err, &ne) {
		t.Fatalf("%q: got error %v, want a *NumberError", in, err)
	}
	if ne.Reason != wantReason {
		t.Errorf("%q: reason %q, want %q", in, ne.Reason, wantReason)
	}
	if ne.Arg != in {
		t.Errorf("%q: error names %q", in, ne.Arg)
	}
}

// Go prints +Inf where C prints inf, and the out-of-range path is what
// produces one — so the spelling is part of the fix, not a detail.
func TestWriteFloatSpellsNonFiniteTheCWay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		spec, goVerb string
		verb         byte
		v            float64
		want         string
	}{
		{"%", "f", 'f', math.Inf(1), "inf"},
		{"%", "f", 'f', math.Inf(-1), "-inf"},
		{"%", "f", 'f', math.NaN(), "nan"},
		{"%", "F", 'F', math.Inf(1), "INF"},
		{"%", "E", 'E', math.NaN(), "NAN"},
		{"%10", "f", 'f', math.Inf(1), "       inf"}, // width applies
		{"%-10", "f", 'f', math.Inf(1), "inf       "},
		{"%.2", "f", 'f', math.Inf(1), "inf"},         // precision does not
		{"%010", "f", 'f', math.Inf(1), "       inf"}, // nor zero-pad
		{"%.2", "f", 'f', 3.14159, "3.14"},            // finite is untouched
	} {
		t.Run(tc.spec+string(tc.verb), func(t *testing.T) {
			t.Parallel()
			var sb strings.Builder
			writeFloat(&sb, tc.spec, tc.verb, tc.goVerb, tc.v)
			if got := sb.String(); got != tc.want {
				t.Errorf("writeFloat(%q, %c, %v) = %q, want %q", tc.spec, tc.verb, tc.v, got, tc.want)
			}
		})
	}
}
