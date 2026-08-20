package repl

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"
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
	out := runSession(t, "echo first >| '"+path+"'\necho second >| '"+path+"'")
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
	runSession(t, "echo first >> '"+path+"'\necho second >> '"+path+"'")
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

// A quoted delimiter makes a heredoc body entirely literal. koi stripped
// the backslash before another backslash, a `$` or a backquote anyway
// (#244) — the *unquoted* heredoc's rule applied to the quoted form — so
// `cat > f <<'EOF'` wrote a file that was not the file it was given, with
// no message and exit 0.
//
// These stay here now that the fix has moved into the interpreter
// (interp.literalHdoc, #259), because they ask the question from the
// outside: whether the shell a person or a harness invokes writes the
// file it was given. The unit coverage of the rule itself lives in
// internal/shell/interp, confirmed case by case against real bash.
//
// Every `want` here is real bash's answer for the same script, taken by
// running it rather than reasoned about, since that is the only thing
// that makes these assertions worth anything.
func TestQuotedHeredocIsLiteral(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{{
		name: "doubled backslash survives",
		src:  "cat <<'X'\nre=\\\\d+\nX",
		want: "re=\\\\d+\n",
	}, {
		name: "escaped dollar survives",
		src:  "cat <<'X'\ncost=\\$5\nX",
		want: "cost=\\$5\n",
	}, {
		name: "escaped backquote survives",
		src:  "cat <<'X'\ncmd=\\`id`\nX",
		want: "cmd=\\`id`\n",
	}, {
		name: "double-quoted delimiter is literal too",
		src:  "cat <<\"X\"\nre=\\\\d+\nX",
		want: "re=\\\\d+\n",
	}, {
		name: "backslash-escaped delimiter is literal too",
		src:  "cat <<\\X\nre=\\\\d+\nX",
		want: "re=\\\\d+\n",
	}, {
		name: "expansions stay literal",
		src:  "V=live\ncat <<'X'\n$V $(echo sub) $((1+1))\nX",
		want: "$V $(echo sub) $((1+1))\n",
	}, {
		// The other half of the fix: applying the literal rule to the
		// unquoted form would be the same bug pointing the other way.
		name: "unquoted heredoc still processes escapes",
		src:  "cat <<X\nre=\\\\d+ \\$v\nX",
		want: "re=\\d+ $v\n",
	}, {
		name: "unquoted heredoc still expands",
		src:  "V=live\ncat <<X\n$V $((1+1))\nX",
		want: "live 2\n",
	}, {
		// The tab stripping for `<<-` runs in the interpreter, over the
		// literal parts of the body — so a fix that restates the body as
		// a quoted part moves it out of reach. The first cut of #244 did
		// exactly that and traded the eaten backslash for kept tabs,
		// which is why this case is here rather than assumed.
		name: "dash form strips tabs and keeps backslashes",
		src:  "cat <<-'X'\n\tre=\\\\d+\n\t\tindented\n\tX",
		want: "re=\\\\d+\nindented\n",
	}, {
		name: "dash form strips tabs only, never spaces",
		src:  "cat <<-'X'\n\t  re=\\\\d+\n\tX",
		want: "  re=\\\\d+\n",
	}, {
		name: "unquoted dash form strips tabs and processes escapes",
		src:  "cat <<-X\n\tre=\\\\d+\n\tX",
		want: "re=\\d+\n",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runSession(t, tc.src); got != tc.want {
				t.Errorf("heredoc body = %q, want %q", got, tc.want)
			}
		})
	}
}

// The symptom rather than the mechanism: `cat > file <<'EOF'` is the
// idiom for writing a file whose content must not be interpreted, and
// #244 was found when a script written that way would not parse — the
// syntax error pointing 30 lines away from the missing backslash.
func TestQuotedHeredocWritesTheFileItWasGiven(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.sh")
	body := "esc=${snippet//\\\\/\\\\\\\\}\nre='\\\\d+'\npath='c:\\\\users'\n"
	runSession(t, "cat > '"+path+"' <<'SH'\n"+body+"SH")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the heredoc wrote no file: %v", err)
	}
	if string(got) != body {
		t.Errorf("file on disk = %q,\n              want %q", got, body)
	}
}

// #244's first fix rewrote the tree *koi* parses, which is every path
// except the two a script is most likely to take: `source` and `eval`
// re-parse inside the interpreter, so the corruption stayed live there
// while every test above passed — a fix that looked complete and covered
// the cases least likely to matter. #259 closed it by moving the rule
// into the interpreter, and this is the test that would have found the
// gap in the first place.
func TestQuotedHeredocSurvivesSourceAndEval(t *testing.T) {
	script := filepath.Join(t.TempDir(), "inner.sh")
	if err := os.WriteFile(script, []byte("cat <<'X'\nre=\\\\d+ and \\$var\nX\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The path is quoted rather than interpolated bare: on Windows it is
	// full of backslashes, which the shell would read as escapes — the
	// test would then fail on the same class of bug it is about.
	quoted := singleQuote(script)
	want := "re=\\\\d+ and \\$var\n"
	for _, tc := range []struct{ name, src string }{
		{"source", ". " + quoted},
		{"eval", `eval "$(cat ` + quoted + `)"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runSession(t, tc.src); got != want {
				t.Errorf("through %s = %q, want %q", tc.name, got, want)
			}
		})
	}
}
