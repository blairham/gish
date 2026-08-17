package repl

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// runIn runs one line in a runner and returns its combined output.
func runIn(t *testing.T, runner *interp.Runner, line string) string {
	t.Helper()
	var out bytes.Buffer
	interp.StdIO(nil, &out, &out)(runner) //nolint:errcheck // in-memory writers
	file, err := syntax.NewParser().Parse(strings.NewReader(line), "test")
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	_ = runner.Run(context.Background(), file)
	return out.String()
}

// TestAliasesExpandOnlyWhenEnabled is the regression test for a builtin
// that was worse than missing.
//
// `alias` records the definition either way, so defining one always
// looks like it worked; without expansion, using it reports "command not
// found". That is the most confusing failure available, and it is what
// every arriving user's rc runs into first.
func TestAliasesExpandOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	t.Run("off by default", func(t *testing.T) {
		t.Parallel()
		runner, err := interp.New()
		if err != nil {
			t.Fatal(err)
		}
		out := runIn(t, runner, "alias hi='echo expanded'; hi")
		if strings.Contains(out, "expanded") {
			t.Errorf("aliases expanded without being enabled: %q", out)
		}
	})

	t.Run("on once enabled", func(t *testing.T) {
		t.Parallel()
		runner, err := interp.New()
		if err != nil {
			t.Fatal(err)
		}
		enableAliases(context.Background(), runner)
		out := runIn(t, runner, "alias hi='echo expanded'; hi")
		if !strings.Contains(out, "expanded") {
			t.Errorf("alias did not expand after enableAliases: %q", out)
		}
	})
}

// The -c and script paths stay bash-correct: expanding aliases there
// would make a script mean different things depending on who ran it,
// and the compat suite pins that path as POSIX-clean.
func TestAliasesStayOffForScripts(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	// A failed lookup is the expected outcome, so the error is not the
	// assertion — the absence of expansion is.
	_ = RunReader(context.Background(), strings.NewReader("alias hi='echo expanded'\nhi\n"), "test",
		interp.StdIO(nil, &out, &out))
	if strings.Contains(out.String(), "expanded") {
		t.Errorf("script path expanded an alias: %q", out.String())
	}
}
