//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The editing mode and the option that reports it (#576).
//
// koi has three spellings of one switch — `set -o vi`, `config editmode
// vi`, KOI_EDIT_MODE — and #163 wired the last of them to the editor
// while the first was rewritten into it, so the interpreter never saw the
// `set`. The editor did what was asked and `set -o`, `shopt -o vi` and
// $SHELLOPTS all reported the other mode. Everything here needs koi as a
// subprocess: the rewrite, the session's startup options and the rc file
// are all the shell's rather than the interpreter's, so none of it is
// reachable from interp's own table.

// editModeLine is the probe: the two option rows, whatever asked for
// them. Read from stdout alone, because bash's forced-interactive shells
// print `cannot set terminal process group` to stderr from a test
// process and that is an artifact of the harness, not an answer.
const editModeLine = `set -o | grep -E '^(emacs|vi) ' | awk '{print $1 "=" $2}'`

// The environment is hermeticEnv's rather than the developer's, because
// a real KOI_EDIT_MODE or a real rc would make the negative case below
// pass or fail for a reason that has nothing to do with the code — and
// bash reads ~/.bashrc under -i, so the oracle needs the same treatment.
func koiStdout(t *testing.T, args []string, env ...string) string {
	t.Helper()
	cmd := exec.Command(buildKoi(t), args...)
	cmd.Env = append(hermeticEnv(t), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("koi %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// An interactive shell starts in emacs mode and a script starts in
// neither, which is bash's answer and was koi's behavior without being
// koi's answer: it edited in emacs and reported emacs off.
func TestInteractiveShellStartsInEmacsMode(t *testing.T) {
	t.Parallel()
	bash := requireBash(t)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"interactive", []string{"-ic", editModeLine}},
		{"script", []string{"-c", editModeLine}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := strings.TrimSpace(shellStdout(t, bash, tc.args))
			// --norc so the machine's own koirc cannot answer for it;
			// bash is given no rc either, since -c reads none.
			got := koiStdout(t, append([]string{"--norc"}, tc.args...))
			if got != want {
				t.Errorf("koi %v reported %q, bash reported %q", tc.args, got, want)
			}
			if want == "" {
				t.Fatal("the oracle printed nothing, so this case cannot detect a wrong answer")
			}
		})
	}
}

func shellStdout(t *testing.T, bin string, args []string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(hermeticEnv(t), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v", bin, args, err)
	}
	return string(out)
}

// koi's own two spellings have no bash oracle — there is no
// KOI_EDIT_MODE in bash — so what is asserted is that the option agrees
// with them, which is the property #576 is about rather than a claim
// about bash's behavior. The mode the option reports is compared to the
// mode bash would report for the equivalent `set -o vi`, above.
func TestKoiEditModeReachesTheOption(t *testing.T) {
	t.Parallel()

	viRC := filepath.Join(t.TempDir(), "koirc")
	if err := os.WriteFile(viRC, []byte("set -o vi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	varRC := filepath.Join(t.TempDir(), "koirc")
	if err := os.WriteFile(varRC, []byte("KOI_EDIT_MODE=vi\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		env  []string
		want string
	}{{
		// The environment, which is how a login shell inherits it.
		name: "KOI_EDIT_MODE in the environment",
		args: []string{"--norc", "-c", editModeLine},
		env:  []string{"KOI_EDIT_MODE=vi"},
		want: "emacs=off\nvi=on",
	}, {
		// The same, in a session that also has the interactive default
		// to overrule: vi wins, and emacs goes off with it.
		name: "KOI_EDIT_MODE beats the interactive default",
		args: []string{"--norc", "-ic", editModeLine},
		env:  []string{"KOI_EDIT_MODE=vi"},
		want: "emacs=off\nvi=on",
	}, {
		// `set -o vi` in an rc is how a vi user actually arrives.
		name: "set -o vi in the rc",
		args: []string{"-ic", editModeLine},
		env:  []string{"KOI_RC=" + viRC},
		want: "emacs=off\nvi=on",
	}, {
		// And `config editmode vi` writes exactly this line.
		name: "KOI_EDIT_MODE in the rc",
		args: []string{"-ic", editModeLine},
		env:  []string{"KOI_RC=" + varRC},
		want: "emacs=off\nvi=on",
	}, {
		// The negative that keeps the four above from passing for the
		// wrong reason: nothing asked for vi, so nothing reports it.
		name: "an untouched interactive shell",
		args: []string{"--norc", "-ic", editModeLine},
		want: "emacs=on\nvi=off",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := koiStdout(t, tc.args, tc.env...); got != tc.want {
				t.Errorf("koi reported %q, want %q", got, tc.want)
			}
		})
	}
}
