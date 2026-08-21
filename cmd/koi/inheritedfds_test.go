//go:build unix

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A descriptor the shell was started with is usable inside it (#419).
//
// bash needs nothing for this: its redirections are real descriptors, so
// an inherited one is simply there. koi models the table, so it has to
// look — and the version of this that was written first was reverted for
// looking wrongly, which is why the negative cases below are as
// load-bearing as the positive ones.

// runWithFDs runs a shell with files placed at specific descriptors.
//
// os/exec numbers ExtraFiles positionally — entry i is descriptor 3+i —
// and a nil entry closes that descriptor in the child, which is what
// lets this hand a shell fd 6 with 3, 4 and 5 shut. That is the same
// layout koi's own table produces for the commands it runs, so this
// harness and the shell agree on what a number means.
func runWithFDs(t *testing.T, shell string, files map[int]*os.File, args ...string) string {
	t.Helper()
	highest := 2
	for fd := range files {
		if fd > highest {
			highest = fd
		}
	}
	cmd := exec.Command(shell, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb"}
	cmd.ExtraFiles = make([]*os.File, highest-2)
	for fd, f := range files {
		cmd.ExtraFiles[fd-3] = f
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run() // a non-zero status is part of what is compared
	return out.String()
}

func openFixture(t *testing.T, body string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestInheritedDescriptorsAreVisible(t *testing.T) {
	t.Parallel()
	bash := requireBash(t)
	koi := buildKoi(t)

	for _, tc := range []struct {
		name string
		fds  []int
		src  string
	}{
		// The redir7 shape from bash's own suite: a descriptor handed to
		// the shell on its invocation, then made standard input.
		{"exec <&N", []int{3}, "exec <&3\ncat\n"},
		{"read -u", []int{3}, "read -u 3 x; echo got=$x\n"},
		// A number well above the first free one, which is where
		// positional renumbering would show up as reading the wrong file.
		{"a high descriptor", []int{6}, "read x <&6; echo got=$x\n"},
		{"passed on to a child", []int{6}, "cat <&6\n"},
		// Two at once, so a table that collapsed them into a list would
		// hand back the wrong one.
		{"two descriptors", []int{4, 7}, "read a <&7; read b <&4; echo \"$a/$b\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := make(map[int]*os.File, len(tc.fds))
			for i, fd := range tc.fds {
				files[fd] = openFixture(t, "line-for-"+strings.Repeat("x", i+1)+"\n")
			}
			want := runWithFDs(t, bash, files, "-c", tc.src)
			// Each shell reads the files from the start, so they are
			// reopened rather than shared.
			for i, fd := range tc.fds {
				files[fd] = openFixture(t, "line-for-"+strings.Repeat("x", i+1)+"\n")
			}
			if got := runWithFDs(t, koi, files, "-c", tc.src); got != want {
				t.Errorf("koi = %q, bash = %q", got, want)
			}
		})
	}
}

// The other half, and the one that reverted the first attempt: a
// descriptor nobody passed is not open, and a redirection to it must
// fail rather than quietly find something. bash's own redir11.sub turns
// on exactly this.
func TestUnpassedDescriptorsStayClosed(t *testing.T) {
	t.Parallel()
	koi := buildKoi(t)

	// fd 6 is passed, so 3, 4 and 5 are the gaps around it — the numbers
	// a positional layout would have used.
	f := openFixture(t, "nothing to see\n")
	for _, src := range []string{
		"a=4; echo x >&$a",
		"read y <&3",
		"exec 4<&5",
	} {
		got := runWithFDs(t, koi, map[int]*os.File{6: f}, "-c", src+"; echo status=$?")
		if !strings.Contains(got, "Bad file descriptor") {
			t.Errorf("%q found a descriptor that is not open: %q", src, got)
		}
	}
}
