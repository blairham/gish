package main

import (
	"strings"
	"testing"
	"time"
)

// atuin's output shape is the contract this bridge depends on, and the
// two ways to get it wrong are silent: a record split on the wrong
// boundary (commands contain newlines) and a duration off by 10^6
// (atuin's --duration is nanoseconds, koi's proto is milliseconds).
// Neither would ever raise an error, so both are tested directly.

func rec(fields ...string) string { return strings.Join(fields, fieldSep) + recordSep }

func TestParseSearchReadsFields(t *testing.T) {
	out := rec("0", "2026-08-16T10:30:00Z", "/home/u/src", "git status") +
		rec("1", "2026-08-16T10:31:00Z", "/tmp", "false")

	got := parseSearch(out)
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(got), got)
	}
	if got[0].Command != "git status" || got[0].Cwd != "/home/u/src" || got[0].ExitCode != 0 {
		t.Errorf("first row = %+v", got[0])
	}
	if got[1].ExitCode != 1 {
		t.Errorf("exit code not carried: %+v", got[1])
	}
	want := time.Date(2026, 8, 16, 10, 30, 0, 0, time.UTC).UnixMilli()
	if got[0].StartedUnixMs != want {
		t.Errorf("time = %d, want %d", got[0].StartedUnixMs, want)
	}
}

// A shell's history is exactly the corpus full of newlines. Splitting on
// them would turn one multi-line command into several bogus rows, which
// is why atuin has --print0 and why this bridge uses it.
func TestParseSearchKeepsMultilineCommands(t *testing.T) {
	multiline := "for f in *; do\n  echo \"$f\"\ndone"
	got := parseSearch(rec("0", "", "/tmp", multiline))

	if len(got) != 1 {
		t.Fatalf("multiline command split into %d rows: %+v", len(got), got)
	}
	if got[0].Command != multiline {
		t.Errorf("command = %q, want %q", got[0].Command, multiline)
	}
}

// The command is the last field, so a tab inside it cannot shift any
// other column.
func TestParseSearchKeepsTabsInCommand(t *testing.T) {
	cmd := "awk -F'\\t' '{print $1\t$2}'"
	got := parseSearch(rec("0", "", "/tmp", cmd))
	if len(got) != 1 || got[0].Command != cmd {
		t.Fatalf("command with tabs mangled: %+v", got)
	}
}

// One malformed record must not cost the user the other rows: search
// feeds an interactive picker, not a data pipeline.
func TestParseSearchSkipsBadRecords(t *testing.T) {
	out := "not-enough-fields" + recordSep +
		rec("0", "", "/tmp", "good command") +
		recordSep + // empty record
		rec("0", "", "/tmp", "") // no command

	got := parseSearch(out)
	if len(got) != 1 || got[0].Command != "good command" {
		t.Fatalf("got %+v, want just the one good row", got)
	}
}

// An unparsable timestamp costs the age column on that row and nothing
// else. atuin renders {time} per the user's own settings, so this has to
// degrade rather than guess.
func TestParseAtuinTimeDegradesToZero(t *testing.T) {
	if got := parseAtuinTime("some human friendly thing"); got != 0 {
		t.Errorf("unparsable time = %d, want 0", got)
	}
	if got := parseAtuinTime(""); got != 0 {
		t.Errorf("empty time = %d, want 0", got)
	}
	for _, s := range []string{
		"2026-08-16T10:30:00Z",
		"2026-08-16 10:30:00 +0000",
		"2026-08-16 10:30:00",
	} {
		if got := parseAtuinTime(s); got == 0 {
			t.Errorf("layout %q did not parse", s)
		}
	}
}

// The nanosecond conversion, asserted on the exact argv the bridge would
// hand atuin. This is the line whose failure mode is a user's entire
// synced history reading as instant, with no error anywhere.
func TestEndConvertsMillisecondsToNanoseconds(t *testing.T) {
	args := endArgs("some-id", 0, 1500) // 1.5s
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--duration 1500000000") {
		t.Errorf("duration not converted to nanoseconds: %q", joined)
	}
	if !strings.Contains(joined, "--exit 0") {
		t.Errorf("exit code missing: %q", joined)
	}

	// A zero duration means "koi did not measure it"; sending
	// --duration 0 would tell atuin the command was instantaneous, so
	// the flag is omitted and atuin computes its own.
	if joined := strings.Join(endArgs("id", 3, 0), " "); strings.Contains(joined, "--duration") {
		t.Errorf("zero duration should omit the flag, got %q", joined)
	}
}

// A missing atuin is a normal state, not a failure: the plugin must
// behave exactly like an uninstalled plugin.
func TestNoAtuinIsNotAnError(t *testing.T) {
	b := &bridge{}
	b.once.Do(func() {}) // pin the probe as "resolved: absent"
	b.available = false

	if _, err := b.start(t.Context(), "echo hi"); err == nil {
		t.Error("start with no atuin returned success")
	}
	if _, ok := b.resolve(); ok {
		t.Error("resolve reported an atuin that is not there")
	}
}

func TestSearchFormatMatchesParser(t *testing.T) {
	// The format string and the parser must agree on field count and
	// order, or every row silently fails to parse.
	fields := strings.Split(searchFormat, fieldSep)
	if len(fields) != 4 {
		t.Fatalf("searchFormat has %d fields, parser expects 4", len(fields))
	}
	if fields[3] != "{command}" {
		t.Errorf("command must be the last field so its tabs cannot shift others, got %q", fields[3])
	}
}
