//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// compareInDir runs one script through both shells in their own copy of
// dir and requires identical stdout and exit status.
//
// A copy each, because these scripts write files: sharing one directory
// would let bash's run seed koi's, and a case about loading $HISTFILE
// would pass on a file the oracle left behind.
func compareInDir(t *testing.T, bashBin, koiBin, script, dir string) {
	t.Helper()
	run := func(bin string) (string, int) {
		t.Helper()
		home := t.TempDir()
		work, err := os.MkdirTemp(dir, "run")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, "-c", script)
		cmd.Dir = work
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "HOME=" + home}
		out, runErr := cmd.Output()
		code := 0
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		} else if runErr != nil {
			t.Fatalf("running %s: %v", bin, runErr)
		}
		return string(out), code
	}
	wantOut, wantCode := run(bashBin)
	gotOut, gotCode := run(koiBin)
	if gotOut != wantOut {
		t.Errorf("stdout =\n%q\nbash =\n%q", gotOut, wantOut)
	}
	if gotCode != wantCode {
		t.Errorf("exit status = %d, bash = %d", gotCode, wantCode)
	}
}

// $HISTFILE on the script path (#432): the load-on-enable and the
// incremental `-a`/`-n` pair, every case run through real bash and
// through koi and required to agree on stdout and exit status.
//
// Each script ends by printing the list, the file, or both, because the
// positions these exercise are invisible otherwise — an append that
// wrote the wrong entries and one that wrote none look identical until
// the file is read back.
//
// HISTFILESIZE is pinned large wherever HISTSIZE is set: assigning
// HISTSIZE with HISTFILESIZE unset makes bash truncate $HISTFILE on the
// spot, which koi deliberately does not implement (see histfile.go), and
// an unpinned case would be measuring that gap rather than these three.
func TestHistoryFileMatchesBash(t *testing.T) {
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	cases := []struct {
		name   string
		script string
	}{
		{"set -o history loads HISTFILE", `printf "one\ntwo\nthree\n" > hf
HISTFILE=hf
set -o history
history`},
		{"a HISTFILE assigned after enabling is never read", `printf "x\ny\n" > hf
set -o history
HISTFILE=hf
history`},
		{"re-enabling does not reload", `printf "f1\n" > hf
HISTFILE=hf; set -o history
set +o history
set -o history
history`},
		{"the preload is stifled and numbers from one", `printf "1\n2\n3\n4\n5\n" > hf
HISTFILE=hf; HISTFILESIZE=100; HISTSIZE=2
set -o history
history`},
		{"a missing HISTFILE loads nothing and says nothing", `HISTFILE=nosuch
set -o history
echo one >/dev/null
history`},
		{"-a writes the entries since the last append", `HISTFILE=hf; set -o history
echo one >/dev/null
history -a
echo two >/dev/null
history -a
echo "--file--"; cat hf`},
		{"-a twice writes only its own second line", `HISTFILE=hf; set -o history
echo one >/dev/null
history -a
history -a
echo "--file--"; cat hf`},
		{"-a does not rewrite the preloaded entries", `printf "p1\np2\n" > hf
HISTFILE=hf; set -o history
echo A >/dev/null
history -a
echo "--file--"; cat hf`},
		{"-a takes an explicit file and leaves HISTFILE alone", `HISTFILE=hf; set -o history
echo one >/dev/null
history -a other
echo "--other--"; cat other
echo "--hf--"; ls hf 2>/dev/null; echo "hf-listed=$?"`},
		{"-a with no HISTFILE and no operand fails", `set -o history
echo one >/dev/null
history -a 2>/dev/null; echo "status=$?"`},
		{"-a after a clear writes only what followed it", `printf "p1\np2\np3\n" > hf
HISTFILE=hf; set -o history
history -c
echo two >/dev/null
history -a
echo "--file--"; cat hf`},
		{"-s entries append like recorded ones", `HISTFILE=hf; set -o history
history -s "stored cmd"
history -a
echo "--file--"; cat hf`},
		{"-n reads what the file grew since the preload", `printf "f1\n" > hf
HISTFILE=hf; set -o history
printf "f2\nf3\n" >> hf
history -n
history`},
		{"-n after -a reads nothing: one counter", `HISTFILE=hf; set -o history
echo one >/dev/null
history -a
history -n
history`},
		{"-n leaves the append position alone", `HISTFILE=hf; set -o history
printf "x1\nx2\n" > hf
history -n
history -a
echo "--file--"; cat hf`},
		{"-n after a clear reads nothing already read", `printf "f1\nf2\n" > hf
HISTFILE=hf; set -o history
history -c
history -n
history`},
		{"-r marks what precedes it as written", `printf "r1\nr2\n" > other
HISTFILE=hf; set -o history
echo A >/dev/null
history -r other
echo B >/dev/null
history -a
echo "--file--"; cat hf`},
		{"-r moves the read position onto its own file", `printf "h1\nh2\nh3\n" > hf
printf "o1\n" > other
HISTFILE=hf
set -o history
history -r other
history -n
history`},
		{"-w writes the whole list", `HISTFILE=hf; set -o history
echo one >/dev/null
history -w
echo "--file--"; cat hf`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A scratch cwd per case: these scripts write hf and other
			// as relative paths, which is also what pins that a history
			// file resolves against the shell's directory.
			dir := t.TempDir()
			compareInDir(t, bashBin, koiBin, tc.script, dir)
		})
	}
}
