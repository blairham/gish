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

// The #41 compatibility suite: the non-interactive contract tools rely
// on when they spawn $SHELL -c (editors, IDEs, cron, ssh, scp).

func runC(t *testing.T, bin string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	if exit, ok := err.(*exec.ExitError); ok { //nolint:errorlint // direct exec result
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out.String(), errb.String(), code
}

func TestNonInteractivePurity(t *testing.T) {
	t.Parallel()
	bin := buildKoi(t)
	env := hermeticEnv(t)

	stdout, stderr, code := runC(t, bin, env, "-c", "echo hi")
	if stdout != "hi\n" {
		t.Errorf("stdout = %q, want exactly %q", stdout, "hi\n")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if code != 0 {
		t.Errorf("exit = %d", code)
	}
	if bytes.ContainsRune([]byte(stdout), 0x1b) {
		t.Error("ANSI escapes leaked into non-interactive output")
	}
}

func TestNonInteractiveExitCodes(t *testing.T) {
	t.Parallel()
	bin := buildKoi(t)
	env := hermeticEnv(t)

	if _, _, code := runC(t, bin, env, "-c", "exit 7"); code != 7 {
		t.Errorf("exit 7 = %d", code)
	}
	if _, _, code := runC(t, bin, env, "-c", "false"); code != 1 {
		t.Errorf("false = %d", code)
	}
}

func TestNonInteractivePosix(t *testing.T) {
	t.Parallel()
	bin := buildKoi(t)
	env := hermeticEnv(t)

	stdout, _, code := runC(t, bin, env, "-c",
		`x=$(printf '%s' ab | tr a-z A-Z); [ "$x" = AB ] && echo "$x-ok"`)
	if code != 0 || stdout != "AB-ok\n" {
		t.Errorf("posix pipeline: stdout=%q code=%d", stdout, code)
	}
}

func TestLoginSourcesProfile(t *testing.T) {
	t.Parallel()
	bin := buildKoi(t)
	env := hermeticEnv(t)

	profile := filepath.Join(t.TempDir(), "profile")
	if err := os.WriteFile(profile, []byte("FROM_PROFILE=sourced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env = append(env, "KOI_PROFILE="+profile)

	// Via the -l flag.
	stdout, _, _ := runC(t, bin, env, "-l", "-c", "echo ${FROM_PROFILE:-no}")
	if stdout != "sourced\n" {
		t.Errorf("-l profile: %q", stdout)
	}

	// Via argv[0] starting with '-', how login(1)/sshd invoke shells.
	cmd := exec.Command(bin, "-c", "echo ${FROM_PROFILE:-no}")
	cmd.Args[0] = "-koi"
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "sourced\n" {
		t.Errorf("argv[0] login: %q", out)
	}

	// Without login, the profile must not be sourced.
	stdout, _, _ = runC(t, bin, env, "-c", "echo ${FROM_PROFILE:-no}")
	if stdout != "no\n" {
		t.Errorf("non-login sourced profile: %q", stdout)
	}
}

func TestScriptFileExecution(t *testing.T) {
	t.Parallel()
	bin := buildKoi(t)
	env := hermeticEnv(t)

	script := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(script, []byte("echo from-script\nexit 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, code := runC(t, bin, env, script)
	if stdout != "from-script\n" || code != 3 {
		t.Errorf("script: stdout=%q code=%d", stdout, code)
	}
}

// bash's `-c` takes positional parameters after the command, and a
// script file takes them after its path (#119's neighborhood: found
// while reducing the substrate gaps).
//
// `koi -c 'rm -- "$1"' _ "$file"` is the safe way to pass a value into
// a snippet without interpolating it — and dropping the parameters
// turns that into `rm --`, which is the kind of silent difference that
// makes a shell untrustworthy for scripting.
func TestPositionalParameters(t *testing.T) {
	bin := buildKoi(t)

	t.Run("-c", func(t *testing.T) {
		out, err := exec.Command(bin, "-c", `echo "0=$0 1=$1 2=$2 count=$#"`, "myname", "alpha", "beta").CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if got, want := strings.TrimSpace(string(out)), "0=myname 1=alpha 2=beta count=2"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("script file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "args.sh")
		if err := os.WriteFile(path, []byte(`echo "1=$1 2=$2 count=$#"`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(bin, path, "alpha", "beta").CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if got, want := strings.TrimSpace(string(out)), "1=alpha 2=beta count=2"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
