package repl

import (
	"context"
	"io"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// runScriptForTest runs a line through a runner wired with the handler
// under test, which is the only way to exercise a CallHandler: the
// rewrite happens inside the interpreter, not beside it.
func runScriptForTest(t *testing.T, out io.Writer, script string) {
	t.Helper()
	runner, err := interp.New(
		interp.StdIO(nil, out, out),
		interp.CallHandler(declCallHandler(passthroughCall)),
	)
	if err != nil {
		t.Fatal(err)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(script), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background(), file); err != nil {
		t.Fatalf("running %q: %v", script, err)
	}
}

// `\export FOO=bar` (#119): a declaration reached as a call.
//
// Escaping or quoting the name changes the *parse* — a declaration
// clause becomes an ordinary command — and the interpreter has no
// builtin behind it, so the shell said "unsupported builtin" and set
// nothing. Defensive init scripts write it deliberately to bypass an
// alias or function of the same name; conda's hook does, which is how
// CI found this.

func TestEscapedDeclarationsAssign(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, script, want string
	}{
		{"escaped export", `\export FOO=bar; printf '%s' "$FOO"`, "bar"},
		{"quoted export", `"export" FOO=bar; printf '%s' "$FOO"`, "bar"},
		// A value with spaces has to survive the round trip through eval.
		{"value with spaces", `\export A="two words"; printf '%s' "$A"`, "two words"},
		// And one with a quote in it, which is where naive re-quoting breaks.
		{"value with a quote", `\export A="it's"; printf '%s' "$A"`, "it's"},
		{"multiple assignments", `\export A=1 B=2; printf '%s%s' "$A" "$B"`, "12"},
		// local must stay local: eval runs in the caller's scope, and a
		// subshell would set the variable where the function cannot see it.
		{"escaped local", `f() { \local x=1; printf '%s' "$x"; }; f`, "1"},
		{"local declared then set", `f() { \local x; x=5; printf '%s' "$x"; }; f`, "5"},
		{"readonly", `\readonly R=fixed; printf '%s' "$R"`, "fixed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			runScriptForTest(t, &out, tt.script)
			if got := out.String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// `local` must not leak out of the function it was declared in — the
// property that would be lost by running the rewrite in a subshell, and
// the one that makes this fix worth more than suppressing the warning.
func TestEscapedLocalStaysLocal(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	runScriptForTest(t, &out, `x=outer; f() { \local x=inner; }; f; printf '%s' "$x"`)
	if got := out.String(); got != "outer" {
		t.Errorf("x = %q after the function returned, want outer", got)
	}
}

// Forms that are not plain assignments are still declined, because
// `\export -f fn` is a real thing to refuse and pretending to support it
// would be a worse answer than the error.
func TestNonAssignmentFormsAreDeclined(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"export", "-f", "somefn"},
		{"export", "-n", "FOO"},
		{"declare", "-A", "map"},
		{"export", "not-a-name=1"},
	} {
		if _, ok := declSource(args); ok {
			t.Errorf("%v was rewritten; it should be left to the interpreter", args)
		}
	}
}
