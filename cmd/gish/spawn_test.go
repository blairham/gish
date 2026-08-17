//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The forms other programs actually spawn a shell with (#217). Each of
// these was a hard failure: the clustered form exited 2 with "flag
// provided but not defined", and the option-after-c form ran a command
// named "-l" and answered 127.
func TestSpawnFormsOtherProgramsUse(t *testing.T) {
	t.Parallel()
	bin := buildGish(t)
	env := hermeticEnv(t)

	profile := filepath.Join(t.TempDir(), "profile")
	if err := os.WriteFile(profile, []byte("FROM_PROFILE=sourced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env = append(env, "GISH_PROFILE="+profile)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"clustered -lc", []string{"-lc", "echo ${FROM_PROFILE:-no}"}, "sourced\n"},
		{"option after -c", []string{"-c", "-l", "echo ${FROM_PROFILE:-no}"}, "sourced\n"},
		{"option before -c", []string{"-l", "-c", "echo ${FROM_PROFILE:-no}"}, "sourced\n"},
		{"plain -c stays non-login", []string{"-c", "echo ${FROM_PROFILE:-no}"}, "no\n"},
		// The command string is an operand, so what follows it is $0, $1…
		{"positional parameters", []string{"-c", "echo $1", "_", "one"}, "one\n"},
		{"-- ends the options", []string{"-c", "--", "echo done"}, "done\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := runC(t, bin, env, tc.args...)
			if code != 0 {
				t.Fatalf("gish %q: exit %d\nstderr: %s", tc.args, code, stderr)
			}
			if stdout != tc.want {
				t.Errorf("gish %q: stdout=%q, want %q", tc.args, stdout, tc.want)
			}
		})
	}
}

// -i means what it means in bash: the rc file runs even for a one-command
// session. Accepting the flag and ignoring it would leave the aliases and
// functions the caller asked for silently absent.
func TestInteractiveFlagSourcesRC(t *testing.T) {
	t.Parallel()
	bin := buildGish(t)
	env := hermeticEnv(t)

	rc := filepath.Join(t.TempDir(), "gishrc")
	if err := os.WriteFile(rc, []byte("FROM_RC=sourced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env = append(env, "GISH_RC="+rc)

	if stdout, stderr, code := runC(t, bin, env, "-ic", "echo ${FROM_RC:-no}"); stdout != "sourced\n" {
		t.Errorf("-ic: stdout=%q code=%d stderr=%s", stdout, code, stderr)
	}
	if stdout, _, _ := runC(t, bin, env, "-c", "echo ${FROM_RC:-no}"); stdout != "no\n" {
		t.Errorf("plain -c sourced the rc file: %q", stdout)
	}
}

// A panic in the interpreter must cost the line, not the session (#217).
//
// The trigger is a real substrate gap — a negated POSIX class in a
// pattern-removal expansion compiles to an invalid regexp, which the
// substrate builds with MustCompile — and it reached gish through a
// vendor block in ~/.profile, which every login invocation sources. A
// terminal emulator profile launches its shell exactly that way, so the
// crash meant no shell at all.
func TestInterpreterPanicDoesNotKillTheShell(t *testing.T) {
	t.Parallel()
	bin := buildGish(t)
	env := hermeticEnv(t)

	const panics = `x="  hi  "; echo "${x%%[![:space:]]*}"`

	t.Run("in a command", func(t *testing.T) {
		t.Parallel()
		stdout, stderr, code := runC(t, bin, env, "-c", panics+"; echo after")
		if strings.Contains(stderr, "goroutine ") {
			t.Errorf("a stack trace reached the user:\n%s", stderr)
		}
		if !strings.Contains(stderr, "internal error") {
			t.Errorf("the failure was not reported: stderr=%q stdout=%q code=%d", stderr, stdout, code)
		}
		if code == 0 {
			t.Error("a contained panic should still be a failure")
		}
	})

	// The one that matters most: a login shell whose profile trips it
	// still starts, still runs the command, and says what went wrong.
	t.Run("in a login profile", func(t *testing.T) {
		t.Parallel()
		profile := filepath.Join(t.TempDir(), "profile")
		if err := os.WriteFile(profile, []byte(panics+"\nFROM_PROFILE=sourced\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := runC(t, bin, append(env, "GISH_PROFILE="+profile), "-lc", "echo alive")
		if stdout != "alive\n" || code != 0 {
			t.Fatalf("a broken profile killed the shell: stdout=%q code=%d stderr=%s", stdout, code, stderr)
		}
		if !strings.Contains(stderr, "internal error") {
			t.Errorf("the profile failure was swallowed: %q", stderr)
		}
	})

	// GISH_DEBUG opts into the stack, for a bug report.
	t.Run("stack behind GISH_DEBUG", func(t *testing.T) {
		t.Parallel()
		_, stderr, _ := runC(t, bin, append(env, "GISH_DEBUG=1"), "-c", panics)
		if !strings.Contains(stderr, "goroutine ") {
			t.Errorf("GISH_DEBUG=1 did not include the stack:\n%s", stderr)
		}
	})
}
