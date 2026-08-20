//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAssignmentErrorEndsTheStringNotTheScript pins #529: bash's split
// between a command string and a script file. `-c` is one input unit, so
// an abandoning error ends it; a script file reports the error and runs
// the next line.
//
// It runs koi as a child because the difference *is* the invocation, and
// each case asserts both halves — a fix that made the file continue
// while also making `-c` continue would be a different bug wearing this
// one's test.
func TestAssignmentErrorEndsTheStringNotTheScript(t *testing.T) {
	t.Parallel()

	koi := buildKoi(t)
	for _, tc := range []struct {
		name    string
		script  string
		message string
	}{
		{
			name:    "associative array without subscripts",
			script:  "declare -A c\nc=( [k]=v four )\necho after\n",
			message: "must use subscript",
		},
		{
			name:    "declare -i value that does not parse",
			script:  "declare -i n\nn=1+\necho after\n",
			message: "arithmetic syntax error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, "s.sh")
			if err := os.WriteFile(path, []byte(tc.script), 0o600); err != nil {
				t.Fatal(err)
			}
			fileOut, _ := exec.Command(koi, path).CombinedOutput()
			if !strings.Contains(string(fileOut), tc.message) {
				t.Errorf("the error was not reported: %q", fileOut)
			}
			if !strings.Contains(string(fileOut), "after") {
				t.Errorf("a script file stopped at the error: %q", fileOut)
			}

			// The same lines as one command string stop, which is what
			// makes this a split rather than a blanket "keep going".
			cmdOut, _ := exec.Command(koi, "-c", strings.ReplaceAll(tc.script, "\n", "; ")).CombinedOutput()
			if strings.Contains(string(cmdOut), "after") {
				t.Errorf("a command string carried on past the error: %q", cmdOut)
			}
		})
	}
}
