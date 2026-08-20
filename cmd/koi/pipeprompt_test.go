//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A shell reading commands from a pipe must not put its prompt on
// stdout (#425).
//
// `koi < script` and `echo cmd | koi` are how tools drive a shell, and
// koi wrote `koi$ ` there on every iteration plus a trailing newline at
// EOF — so every such invocation handed its caller corrupted output.
// bash suppresses the prompt entirely when stdin is not a terminal, and
// under a forced `-i` writes it to stderr instead.
//
// stdout is compared against bash; stderr is not, because koi keeps its
// own prompt string and diagnostic shapes (#120). What is asserted about
// stderr is only the property that makes `-i` worth honoring: the prompt
// went somewhere, and that somewhere is not stdout.
func TestPipedStdinPromptsStayOffStdout(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	cases := []struct {
		name   string
		argv   []string
		script string
	}{
		{"a piped script", nil, "echo hi\n"},
		{"several commands", nil, "echo one\necho two\n"},
		{"a continuation line", nil, "for x in a b\ndo echo $x\ndone\n"},
		{"forced interactive", []string{"-i"}, "echo hi\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(bin string) (string, string) {
				t.Helper()
				home := t.TempDir()
				path := filepath.Join(home, "s")
				if err := os.WriteFile(path, []byte(tc.script), 0o600); err != nil {
					t.Fatal(err)
				}
				in, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer in.Close()
				var stdout, stderr strings.Builder
				cmd := exec.Command(bin, tc.argv...)
				cmd.Dir = home
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "HOME=" + home, "TERM=dumb"}
				cmd.Stdin = in
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				_ = cmd.Run() // a nonzero status is not what this measures
				return stdout.String(), stderr.String()
			}

			wantOut, _ := run(bashBin)
			gotOut, gotErr := run(koiBin)
			if gotOut != wantOut {
				t.Errorf("stdout = %q, bash = %q", gotOut, wantOut)
			}
			if strings.Contains(gotOut, "koi$") || strings.Contains(gotOut, "> ") {
				t.Errorf("a prompt reached stdout: %q", gotOut)
			}
			// Under -i the prompt must still exist, on stderr. Asserting
			// only its absence from stdout would pass just as well for a
			// shell that stopped prompting altogether.
			if len(tc.argv) > 0 && !strings.Contains(gotErr, "koi$") {
				t.Errorf("-i printed no prompt anywhere: stderr = %q", gotErr)
			}
			if len(tc.argv) == 0 && gotErr != "" {
				t.Errorf("a non-interactive pipe wrote to stderr: %q", gotErr)
			}
		})
	}
}
