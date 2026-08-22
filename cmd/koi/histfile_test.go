//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// $HISTFILE is written when the session ends (#737).
//
// koi wrote it on no path at all, so #432's round trip was one-way: a
// script that set HISTFILE and turned history on left the file exactly
// as it found it. bash writes it for a **non-interactive** shell too,
// which is the part worth pinning rather than assuming — its own
// history5.sub ends `unset HISTFILE  # suppress writing history file`,
// a line that means nothing unless the write happens.
//
// These cases cannot go through compareInDir: what they measure is the
// file *after* the shell has exited, and the write happens after the
// last statement could have printed it. So each shell gets its own
// directory, a seeded history file, and the file is read back once the
// process is gone.
//
// The seed is what makes the whole suite an assertion about safety
// rather than about mechanics — a case that overwrote where bash
// appends would lose `pre1`/`pre2` and say so by name.
func TestHistoryFileWrittenAtExitMatchesBash(t *testing.T) {
	if testing.Short() {
		t.Skip("differential skipped in -short")
	}
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	cases := []struct {
		name, script string
	}{
		{"a script writes what it recorded", `HISTFILE=./hf
set -o history
echo a >/dev/null
echo b >/dev/null`},
		{"history never turned on writes nothing", `HISTFILE=./hf
echo a >/dev/null`},
		{"turning history off before the end suppresses the write", `HISTFILE=./hf
set -o history
echo a >/dev/null
set +o history
echo b >/dev/null`},
		{"unsetting HISTFILE suppresses the write", `HISTFILE=./hf
set -o history
echo a >/dev/null
unset HISTFILE`},
		{"the entries appended are the ones since the last -a", `HISTFILE=./hf
set -o history
echo a >/dev/null
history -a
echo tail >/dev/null`},
		{"an -a with nothing after it leaves the exit write nothing to do", `HISTFILE=./hf
set -o history
echo a >/dev/null
history -a`},
		{"-w does not move the append position", `HISTFILE=./hf
set -o history
echo a >/dev/null
history -w`},
		{"a clear leaves only what followed it", `HISTFILE=./hf
set -o history
echo a >/dev/null
history -c
echo b >/dev/null`},
		{"a clear with nothing after it writes nothing", `HISTFILE=./hf
set -o history
echo a >/dev/null
history -c`},
		{"an explicit exit is itself recorded", `HISTFILE=./hf
set -o history
echo a >/dev/null
exit 3`},
		{"the write happens after the EXIT trap, so a removed file comes back", `HISTFILE=./hf
set -o history
echo a >/dev/null
trap 'rm -f ./hf' 0
echo b >/dev/null`},
		{"HISTFILESIZE truncates the file the write just made", `HISTFILE=./hf
HISTFILESIZE=3
set -o history
echo a >/dev/null
echo b >/dev/null`},
		// The two branches of the rule that makes this safe. bash writes
		// the whole list — losing the seed — only when a HISTSIZE trim
		// dropped entries that were never written, and `shopt -s
		// histappend` turns even that into an append. HISTFILESIZE is
		// pinned large in both so the truncation is not what is being
		// measured.
		{"a stifled list past the append position replaces the file", `HISTFILE=./hf
HISTSIZE=1; HISTFILESIZE=100
set -o history
echo a >/dev/null
echo b >/dev/null
echo c >/dev/null`},
		{"histappend keeps the file even then", `HISTFILE=./hf
HISTSIZE=1; HISTFILESIZE=100
shopt -s histappend
set -o history
echo a >/dev/null
echo b >/dev/null
echo c >/dev/null`},
		{"a relative HISTFILE follows the shell's directory", `HISTFILE=./hf
set -o history
cd sub
echo a >/dev/null`},
		// The same resolution on the other write $HISTFILE has: #491's
		// truncate-on-assignment read the path against the *Go process's*
		// directory, so after a `cd` it shortened whatever file happened
		// to share the name up there. No history is turned on here, so
		// the only thing that can touch a file is the truncation.
		{"HISTFILESIZE truncates the file the shell's directory names", `HISTFILE=./hf
cd sub
HISTFILESIZE=1`},
		{"a HISTFILE assigned late is the one written", `set -o history
HISTFILE=./hf
echo a >/dev/null`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			compareHistFileAtExit(t, bashBin, koiBin, tc.script)
		})
	}
}

// The same write on the path a tool actually drives: `cat setup.sh |
// koi` and `koi < setup.sh` are a different loop from `-c` and a
// different one again from a script file, and each has its own way out
// (EOF, `exit`, a parse error), so the write is asserted per path rather
// than assumed to be shared.
func TestHistoryFileWrittenAtExitOnEveryInputPath(t *testing.T) {
	if testing.Short() {
		t.Skip("differential skipped in -short")
	}
	bashBin := requireBash(t)
	koiBin := buildKoi(t)

	const script = `HISTFILE=./hf
set -o history
echo a >/dev/null
echo b >/dev/null
`
	for _, tc := range []struct {
		name string
		argv func(dir string) ([]string, bool) // args, feed on stdin
	}{
		{"piped into standard input", func(string) ([]string, bool) { return nil, true }},
		{"a script file named on the command line", func(string) ([]string, bool) {
			return []string{"./s.sh"}, false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := func(bin string) string {
				t.Helper()
				dir := t.TempDir()
				seedHistFile(t, dir)
				if err := os.WriteFile(filepath.Join(dir, "s.sh"), []byte(script), 0o600); err != nil {
					t.Fatal(err)
				}
				args, pipe := tc.argv(dir)
				cmd := exec.Command(bin, args...)
				cmd.Dir = dir
				cmd.Env = histEnv(dir)
				if pipe {
					cmd.Stdin = strings.NewReader(script)
				}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s failed: %v\n%s", bin, err, out)
				}
				return readHistFile(t, dir)
			}
			got, oracle := run(koiBin), run(bashBin)
			if got != oracle {
				t.Errorf("$HISTFILE differs from bash\n  bash: %q\n  koi:  %q", oracle, got)
			}
		})
	}
}

// compareHistFileAtExit runs one script through both shells in their own
// directory, each seeded with the same two-line history file, and
// requires the file left behind and the exit status to agree.
func compareHistFileAtExit(t *testing.T, bashBin, koiBin, script string) {
	t.Helper()
	run := func(bin string) (string, int) {
		t.Helper()
		dir := t.TempDir()
		seedHistFile(t, dir)
		cmd := exec.Command(bin, "-c", script)
		cmd.Dir = dir
		cmd.Env = histEnv(dir)
		out, runErr := cmd.Output()
		code := 0
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		} else if runErr != nil {
			t.Fatalf("running %s: %v", bin, runErr)
		}
		if len(out) > 0 {
			t.Fatalf("%s printed on stdout, which these cases must not: %q", bin, out)
		}
		return readHistFile(t, dir), code
	}
	wantFile, wantCode := run(bashBin)
	gotFile, gotCode := run(koiBin)
	if gotFile != wantFile {
		t.Errorf("$HISTFILE after exit =\n%q\nbash =\n%q", gotFile, wantFile)
	}
	if gotCode != wantCode {
		t.Errorf("exit status = %d, bash = %d", gotCode, wantCode)
	}
}

// seedHistFile writes the two lines every case starts from, in the
// working directory and in the `sub` a case may cd into — so a write
// that resolved the relative path against the wrong directory shows up
// as content rather than as a missing file.
func seedHistFile(t *testing.T, dir string) {
	t.Helper()
	const seed = "pre1\npre2\n"
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "hf"), filepath.Join(dir, "sub", "hf")} {
		if err := os.WriteFile(p, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// readHistFile reads back both seeded files, labeled, so a case that
// wrote the wrong one is a diff rather than a silence.
func readHistFile(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{"hf", filepath.Join("sub", "hf")} {
		b.WriteString("--" + name + "--\n")
		data, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case os.IsNotExist(err):
			b.WriteString("(absent)\n")
		case err != nil:
			t.Fatal(err)
		default:
			b.Write(data)
		}
	}
	return b.String()
}

// histEnv is a hermetic environment for these runs. HOME is the case's
// own directory rather than the developer's, which is the rule the
// preload test in internal/repl states at length: a case about $HISTFILE
// that inherited the real one would read, and now write, a real shell
// history.
func histEnv(dir string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"HOME=" + dir,
		"TMPDIR=" + dir,
		"KOI_WELCOME=off",
	}
}
