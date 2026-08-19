//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The call-frame stack, differentially, with real files (#266, #250).
//
// interp's own table covers the semantics case by case, but it cannot
// cover the part these variables exist for: naming a *file*. That harness
// gives bash the script on stdin and koi no parse name at all, so the two
// disagree about what to call the input — a $0 divergence (#120), not a
// frame one. Here both shells are handed the same script on disk and
// their whole output is compared, so a wrong file or a wrong line fails.
//
// bash is the oracle: nothing below encodes what bash ought to print.

// bashBin is the differential oracle. It defers to requireBash so there is
// one way to pick it: this used to do its own LookPath and therefore
// ignored KOI_TEST_BASH, which is the variable that exists so a CI-only
// failure against macOS's bash 3.2 can be reproduced locally. Two
// lookalike helpers disagreeing about that is how one shipped (#287).
func bashBin(t *testing.T) string {
	t.Helper()
	return requireBash(t)
}

// runScript runs one script file under a shell from a neutral directory,
// with the files it sources beside it.
func runScript(t *testing.T, shell, path string) string {
	t.Helper()
	cmd := exec.Command(shell, path)
	cmd.Dir = filepath.Dir(path)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb"}
	out, _ := cmd.CombinedOutput() // a non-zero status is part of the comparison
	return string(out)
}

// write drops the files of one case into a fresh directory and returns
// the entry point.
func write(t *testing.T, files map[string]string, entry string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, entry)
}

// The paths differ between the two runs because each gets its own temp
// directory, so they are reduced to base names before comparing. What is
// being checked is *which* file each frame names, not where it lives.
func baseNames(s, dir string) string {
	return strings.ReplaceAll(s, dir+string(filepath.Separator), "")
}

func TestFramesMatchBash(t *testing.T) {
	t.Parallel()
	koi, bash := buildKoi(t), bashBin(t)

	tests := []struct {
		name  string
		files map[string]string
	}{{
		// The whole point of the issue: a hand-rolled `die` naming the
		// file and line of the code that called it.
		name: "a die helper names its caller",
		files: map[string]string{"main.sh": `die() { echo "${BASH_SOURCE[1]}:${BASH_LINENO[0]}: ${FUNCNAME[1]}: $*"; }
f() {
  die boom
}
f
die from-top
`},
	}, {
		// BASH_SOURCE names where a function was *defined*, so a helper
		// in a library reports the library and not its caller's file.
		name: "a sourced library keeps its own name",
		files: map[string]string{
			"lib.sh": `show() {
  echo "FUNCNAME=(${FUNCNAME[@]})"
  echo "SOURCE=(${BASH_SOURCE[@]})"
  echo "LINENO=(${BASH_LINENO[@]})"
}
`,
			"main.sh": `. ./lib.sh
outer() {
  show
}
outer
`,
		},
	}, {
		// A `source` is a frame of its own, named "source", and the
		// library's top level sees itself in BASH_SOURCE[0].
		name: "a source frame is a frame",
		files: map[string]string{
			"lib.sh": `echo "lib top: SOURCE=(${BASH_SOURCE[@]}) LINENO=(${BASH_LINENO[@]})"
g() {
  echo "in g: FUNCNAME=(${FUNCNAME[@]})"
  echo "in g: SOURCE=(${BASH_SOURCE[@]})"
  echo "in g: caller0=[$(caller 0)]"
  echo "in g: caller1=[$(caller 1)]"
  echo "in g: caller2=[$(caller 2)]"
}
g
`,
			"main.sh": `echo "main top: SOURCE=(${BASH_SOURCE[@]}) LINENO=(${BASH_LINENO[@]}) FUNCNAME=(${FUNCNAME[@]})"
. ./lib.sh
echo "after: SOURCE=(${BASH_SOURCE[@]})"
`,
		},
	}, {
		// The idiom every runnable-library script uses to tell whether it
		// was executed or sourced. It reads BASH_SOURCE[0] at the top
		// level, which was empty.
		name: "the sourced-or-executed idiom",
		files: map[string]string{"main.sh": `if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then echo executed; else echo sourced; fi
`},
	}, {
		name: "caller walks the stack",
		files: map[string]string{"main.sh": `g() {
  caller 0
  caller 1
  caller 2
  caller
}
f() {
  g
}
f
caller 0; echo "top rc=$?"
`},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			koiPath := write(t, tc.files, "main.sh")
			bashPath := write(t, tc.files, "main.sh")
			got := baseNames(runScript(t, koi, koiPath), filepath.Dir(koiPath))
			want := baseNames(runScript(t, bash, bashPath), filepath.Dir(bashPath))
			if got != want {
				t.Errorf("koi and bash disagree:\nkoi:\n%s\nbash:\n%s", got, want)
			}
		})
	}
}
