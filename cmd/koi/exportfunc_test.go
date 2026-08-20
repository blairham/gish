//go:build unix

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// An exported function has to reach a real child shell (#387), which is
// the half no in-process case can show: the definition travels through
// the environment, so the test needs an actual subprocess on both sides.
//
// Differential where it can be — the round trip is compared against
// bash's — and koi-only for the import direction, where the environment
// is built by hand and both shells are asked the same question.

func runSh(t *testing.T, bin string, env []string, script string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, "-c", script)
	cmd.Env = append(append(os.Environ(), "LC_ALL=C"), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if err != nil {
		if !asExitError(err, &ee) {
			t.Fatalf("running %s: %v", bin, err)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

func TestExportedFunctionReachesChild(t *testing.T) {
	koi, bash := buildKoi(t), bashBin(t)

	// $SH is each shell's own path, so the child is the same shell as
	// the parent on both sides of the comparison.
	const script = `foo(){ echo exportfunc ok; }
export -f foo
"$SH" -c foo
export -nf foo
"$SH" -c foo 2>/dev/null || echo gone`

	koiOut, koiCode := runSh(t, koi, []string{"SH=" + koi}, script)
	bashOut, bashCode := runSh(t, bash, []string{"SH=" + bash}, script)
	if koiOut != bashOut || koiCode != bashCode {
		t.Errorf("export -f round trip differs from bash\n  bash: %q (exit %d)\n  koi:  %q (exit %d)",
			bashOut, bashCode, koiOut, koiCode)
	}
	if !strings.Contains(koiOut, "exportfunc ok") {
		t.Errorf("the exported function never ran in the child: %q", koiOut)
	}
}

func TestFunctionImportedFromEnvironment(t *testing.T) {
	koi, bash := buildKoi(t), bashBin(t)

	// The environment entry is bash's own spelling, built by hand: this
	// is how a function defined in one shell arrives in another.
	env := []string{`BASH_FUNC_zz%%=() { echo imported; }`}
	koiOut, koiCode := runSh(t, koi, env, "zz")
	bashOut, bashCode := runSh(t, bash, env, "zz")
	if koiOut != bashOut || koiCode != bashCode {
		t.Errorf("environment import differs from bash\n  bash: %q (exit %d)\n  koi:  %q (exit %d)",
			bashOut, bashCode, koiOut, koiCode)
	}

	// A definition that does not parse must not stop the shell from
	// starting: this is untrusted input, and refusing to run is a worse
	// answer than ignoring the entry (the CVE-2014-6271 shape).
	bad := []string{`BASH_FUNC_broken%%=() { echo unclosed`}
	out, code := runSh(t, koi, bad, "echo alive")
	if code != 0 || !strings.Contains(out, "alive") {
		t.Errorf("an unparsable exported function stopped the shell: %q (exit %d)", out, code)
	}
}
