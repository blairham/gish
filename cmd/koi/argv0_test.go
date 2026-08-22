//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `$0` for a shell reading its commands from standard input (#744).
//
// koi answered the literal `gosh` there — the substrate's own upstream
// binary name, the fallback in `internal/shell/interp`'s
// `Runner.lookupVar` — because koi never set a parse name on that path
// and the other three shapes did. bash answers its own binary path;
// koi's rule is #120's, so the answer is `koi`, the same as `-c`
// already gives.
//
// The comparison is not against bash for the two non-file shapes,
// deliberately: `$0` is a decided divergence and a differential case
// there would be red forever. What *is* compared to bash is the shape of
// the answer — that the stdin path and the `-c` path agree with each
// other, as they do in bash, and that a script file names its path in
// both shells.
func TestArgv0PerInvocationShape(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test skipped in -short")
	}
	t.Parallel()

	koi := buildKoi(t)
	const probe = `echo "0=[$0] argv0=[$BASH_ARGV0]"` + "\n"

	// The stdin path has two spellings and both reached the same gap:
	// `cat setup.sh | koi` is how a provisioning script feeds a shell,
	// and `koi < setup.sh` is the same shell with the file on fd 0.
	stdin := func(t *testing.T, redirect bool) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "probe.sh")
		if err := os.WriteFile(path, []byte(probe), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck // read-only fixture
		cmd := exec.Command(koi)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "KOI_WELCOME=off"}
		if redirect {
			cmd.Stdin = f
		} else {
			cmd.Stdin = strings.NewReader(probe)
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("koi failed: %v", err)
		}
		return strings.TrimSpace(string(out))
	}

	want := "0=[koi] argv0=[koi]"
	for _, tc := range []struct {
		name     string
		redirect bool
	}{
		{"piped into standard input", false},
		{"standard input redirected from a file", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := stdin(t, tc.redirect)
			if strings.Contains(got, "gosh") {
				t.Fatalf("$0 leaks the substrate's own binary name: %q", got)
			}
			if got != want {
				t.Errorf("$0 = %q, want %q — the stdin path answers what -c answers (#120)", got, want)
			}
		})
	}

	t.Run("a command string answers the same thing", func(t *testing.T) {
		t.Parallel()
		out, err := exec.Command(koi, "-c", probe).Output()
		if err != nil {
			t.Fatalf("koi failed: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("$0 = %q, want %q", got, want)
		}
	})

	t.Run("a script file still names its path, as bash does", func(t *testing.T) {
		t.Parallel()
		bash := requireBash(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "probe.sh")
		if err := os.WriteFile(path, []byte(probe), 0o600); err != nil {
			t.Fatal(err)
		}
		run := func(bin string) string {
			t.Helper()
			cmd := exec.Command(bin, "./probe.sh")
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "KOI_WELCOME=off"}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s failed: %v", bin, err)
			}
			return strings.TrimSpace(string(out))
		}
		got, oracle := run(koi), run(bash)
		if got != oracle {
			t.Errorf("script $0 differs from bash\n  bash: %q\n  koi:  %q", oracle, got)
		}
	})

	// BASH_ARGV0 is $0's writable view (#408), and it must still win over
	// the parse name the fix installs — otherwise the fix would have
	// closed one hole by making the variable read-only in practice.
	t.Run("BASH_ARGV0 still renames the shell on the stdin path", func(t *testing.T) {
		t.Parallel()
		cmd := exec.Command(koi)
		cmd.Stdin = strings.NewReader("BASH_ARGV0=renamed\necho \"0=[$0]\"\n")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "KOI_WELCOME=off"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("koi failed: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "0=[renamed]" {
			t.Errorf("$0 = %q, want %q", got, "0=[renamed]")
		}
	})
}
