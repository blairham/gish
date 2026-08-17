package repl

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

// runSession runs src through the script path — the one tools take when
// they spawn `$SHELL -c` — and returns its combined output. RunReader is
// used rather than a hand-wired runner because the rewrite happens
// between parse and run, so a test that parses for itself would test
// nothing.
func runSession(t *testing.T, src string) string {
	t.Helper()
	var out bytes.Buffer
	err := RunReader(context.Background(), strings.NewReader(src), "test",
		interp.StdIO(nil, &out, &out))
	if err != nil {
		if _, ok := err.(interp.ExitStatus); !ok {
			t.Fatalf("running %q: %v", src, err)
		}
	}
	return out.String()
}

// `>|` used to fail with no message at all, exit 1, and no file — the
// worst available shape, since the script reads as if it worked.
func TestClobberRedirectWritesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot")
	out := runSession(t, "echo first >| "+path+"\necho second >| "+path)
	if out != "" {
		t.Errorf("clobber redirect wrote to the terminal: %q", out)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the redirect created no file: %v", err)
	}
	// The second write truncates, exactly as `>` would.
	if string(got) != "second\n" {
		t.Errorf("file contents = %q, want %q", got, "second\n")
	}
}

func TestClobberAppendKeepsAppending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	runSession(t, "echo first >> "+path+"\necho second >> "+path)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\nsecond\n" {
		t.Errorf("append = %q, want %q", got, "first\nsecond\n")
	}
}

// bash's two shapes, which callers depend on: bare -F prints the
// restoring command per function, -F with a name prints just the name
// and reports absence through the exit status.
func TestDeclareFListsFunctions(t *testing.T) {
	out := runSession(t, "beta() { :; }\nalpha() { :; }\ndeclare -F")
	want := "declare -f alpha\ndeclare -f beta\n"
	if out != want {
		t.Errorf("declare -F = %q, want %q", out, want)
	}
}

func TestDeclareFNamesOneFunction(t *testing.T) {
	out := runSession(t, "f() { :; }\ndeclare -F f")
	if out != "f\n" {
		t.Errorf("declare -F f = %q, want %q", out, "f\n")
	}
}

func TestDeclareFReportsMissingByStatus(t *testing.T) {
	out := runSession(t, "f() { :; }\ndeclare -F nope || echo rc=$?")
	if out != "rc=1\n" {
		t.Errorf("declare -F nope = %q, want %q", out, "rc=1\n")
	}
}

// The rewrite is narrow on purpose: every other declaration form has to
// keep reaching the interpreter's own clause handling.
func TestOtherDeclarationsAreUntouched(t *testing.T) {
	out := runSession(t, `declare -x FOO=bar
echo "$FOO"
f() { declare -F >/dev/null; local inner=yes; echo "$inner"; }
f`)
	if out != "bar\nyes\n" {
		t.Errorf("declarations = %q, want %q", out, "bar\nyes\n")
	}
}

// shopt -p prints options as the commands that restore them, which is
// the whole reason a snapshotting tool calls it.
func TestShoptPrintsRestoringCommands(t *testing.T) {
	out := runSession(t, "shopt -p dotglob\nshopt -s dotglob\nshopt -p dotglob")
	want := "shopt -u dotglob\nshopt -s dotglob\n"
	if out != want {
		t.Errorf("shopt -p = %q, want %q", out, want)
	}
}

func TestShoptPrintsEveryOptionWithoutNames(t *testing.T) {
	out := runSession(t, "shopt -p")
	if !strings.Contains(out, "shopt -u dotglob\n") {
		t.Errorf("shopt -p omitted a known option:\n%s", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "shopt -s ") && !strings.HasPrefix(line, "shopt -u ") {
			t.Errorf("shopt -p emitted a line that is not a command: %q", line)
		}
	}
}
