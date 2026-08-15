package repl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFootgunWarnings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want string // substring of some warning; "" means no warnings
	}{
		{"unquoted var under rm", `rm $dir/*`, "unquoted $dir under rm"},
		{"quoted var under rm", `rm "$dir"/*`, ""},
		{"braced unquoted var", `rm -rf ${build}/out`, "unquoted $build"},
		{"unquoted var under safe command", `echo $dir`, ""},
		{"positional under rm", `rm $1`, "unquoted $1"},
		{"exit status is split-safe", `rm $?`, ""},

		{"cd sequenced before rm", `cd /tmp/build; rm -rf *`, "cd can fail"},
		{"cd chained before rm", `cd /tmp/build && rm -rf *`, ""},
		{"cd sequenced before echo", `cd /tmp/build; echo hi`, ""},
		{"cd-rm inside a block", "if true; then\n cd /tmp; rm -rf x\nfi", "cd can fail"},

		{"unquoted var in single bracket", `[ $x = y ] && echo ok`, "unquoted $x in [ ]"},
		{"quoted var in single bracket", `[ "$x" = y ] && echo ok`, ""},
		{"unquoted var in double bracket", `[[ $x = y ]] && echo ok`, ""},
		{"test builtin", `test $x = y`, "unquoted $x in [ ]"},

		{"useless cat", `cat notes.txt | grep todo`, "useless cat"},
		{"cat of stdin piped", `cat | grep todo`, ""},
		{"cat with flag", `cat -n notes.txt | grep todo`, ""},
		{"cat into non-filter", `cat notes.txt | xargs rm`, ""},
		{"useless cat in longer pipe", `cat notes.txt | grep todo | wc -l`, "useless cat"},

		{"redirect truncates input", `sort names.txt > names.txt`, "truncates it before sort"},
		{"redirect to other file", `sort names.txt > sorted.txt`, ""},
		{"append does not truncate", `sort names.txt >> names.txt`, ""},

		{"mid-edit parse error stays quiet", `rm "$unclosed`, ""},
		{"empty line", ``, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			warns := footgunWarnings(tt.line)
			if tt.want == "" {
				if len(warns) != 0 {
					t.Fatalf("footgunWarnings(%q) = %q, want none", tt.line, warns)
				}
				return
			}
			joined := strings.Join(warns, "\n")
			if !strings.Contains(joined, tt.want) {
				t.Errorf("footgunWarnings(%q) = %q, want substring %q", tt.line, warns, tt.want)
			}
		})
	}
}

func TestFootgunWarningsCapped(t *testing.T) {
	t.Parallel()

	line := `rm $a; rm $b; [ $c = y ]; cat f | grep x; sort f > f`
	if warns := footgunWarnings(line); len(warns) > maxNativeWarnings {
		t.Errorf("got %d warnings, cap is %d: %q", len(warns), maxNativeWarnings, warns)
	}
}

func TestLintFnKnobAndStyle(t *testing.T) {
	runner := newTestRunner(t)

	// Default: on, styled when color is allowed.
	warns := lintFn(runner, true)(`rm $dir`)
	if len(warns) != 1 || !strings.Contains(warns[0], "\x1b[2;33m") {
		t.Fatalf("styled warnings = %q", warns)
	}
	// No color: plain text with a plain prefix.
	warns = lintFn(runner, false)(`rm $dir`)
	if len(warns) != 1 || !strings.HasPrefix(warns[0], "warning: ") || strings.Contains(warns[0], "\x1b") {
		t.Fatalf("plain warnings = %q", warns)
	}

	// GISH_LINT=off silences everything, live.
	if err := runner.Run(t.Context(), parseLine(t, `GISH_LINT=off`)); err != nil {
		t.Fatal(err)
	}
	if warns = lintFn(runner, true)(`rm $dir`); warns != nil {
		t.Errorf("GISH_LINT=off warnings = %q", warns)
	}
}

// TestShellcheckWarnings drives the pass with a stub shellcheck binary:
// hermetic, no dependency on a real install.
func TestShellcheckWarnings(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "shellcheck")
	script := `#!/bin/sh
echo '[{"line":2,"column":5,"level":"warning","code":2086,"message":"Double quote to prevent globbing and word splitting."},` +
		`{"line":3,"column":1,"level":"style","code":2006,"message":"Use $(...) notation."}]'
exit 1
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	warns := shellcheckWarnings(context.Background(), "for f in $(ls)\ndo rm $f\ndone")
	if len(warns) != 1 {
		t.Fatalf("warns = %q, want the style finding filtered out", warns)
	}
	if !strings.Contains(warns[0], "SC2086") || !strings.Contains(warns[0], "2:5") {
		t.Errorf("warning missing code or position: %q", warns[0])
	}
}

func TestShellcheckMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if warns := shellcheckWarnings(context.Background(), "a\nb"); warns != nil {
		t.Errorf("no shellcheck installed should mean no warnings, got %q", warns)
	}
}
