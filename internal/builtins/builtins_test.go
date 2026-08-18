package builtins_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/builtins"
)

func run(t *testing.T, src string) (string, error) {
	t.Helper()
	var out strings.Builder
	runner, err := interp.New(
		interp.StdIO(nil, &out, &out),
		interp.ExecHandlers(builtins.ExecHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "test")
	if err != nil {
		t.Fatal(err)
	}
	rerr := runner.Run(context.Background(), file)
	return out.String(), rerr
}

func TestBuiltinsListsSections(t *testing.T) {
	t.Parallel()

	out, err := run(t, "builtins")
	if err != nil {
		t.Fatalf("builtins: %v", err)
	}
	for _, want := range []string{
		"koi builtins:", "builtins",
		"shell builtins:", " cd ", " echo ",
		"recognized but not yet supported:", "jobs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestNonBuiltinFallsThrough(t *testing.T) {
	t.Parallel()

	// A plain external command must still work through the middleware.
	out, err := run(t, "echo passthrough && true")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	if !strings.Contains(out, "passthrough") {
		t.Errorf("output = %q", out)
	}
}

// TestInterpListsAccurate guards the static lists against upstream mvdan
// changes: every "unsupported" name must still fail as unsupported, and a
// sample of "implemented" names must still run. When this fails after a
// mvdan upgrade, update the lists in builtins.go.
func TestInterpListsAccurate(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"jobs", "fg", "bg", "umask", "times", "fc"} {
		var errOut strings.Builder
		runner, err := interp.New(interp.StdIO(nil, io.Discard, &errOut))
		if err != nil {
			t.Fatal(err)
		}
		file, err := syntax.NewParser().Parse(strings.NewReader(name), "test")
		if err != nil {
			t.Fatal(err)
		}
		rerr := runner.Run(context.Background(), file)
		if rerr == nil || !strings.Contains(errOut.String(), "unsupported builtin") {
			t.Errorf("%s: expected unsupported builtin (err=%v, stderr=%q) — update builtins.go lists",
				name, rerr, errOut.String())
		}
	}

	for _, src := range []string{"pwd", "echo x", "true", "type type"} {
		if _, err := run(t, src); err != nil {
			t.Errorf("%q: implemented builtin failed: %v", src, err)
		}
	}
}
