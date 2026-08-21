//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The shell's own descriptors, named as files (#645).
//
// `/dev/stdin`, `/dev/fd/N` and their siblings are not ordinary paths:
// opening one is a dup of a descriptor the shell holds. bash gets that
// for free, since its redirections are real dup2 calls on real
// descriptors, while koi models the table — so an open through the
// operating system answered with the descriptor the *process* was
// started with, which inside a pipeline is not the one the shell has.
//
// These are differential and run a real script *file*, which is the
// arrangement where the bug was worst: `interp`'s own table feeds bash
// on standard input, one of the very shapes under test, and the visible
// damage depended on what the process's fd 0 happened to hold — a
// terminal blocked, a redirected file was replayed, /dev/null went
// silently missing.

// readingShape is one of the four ways the shell can be reading its
// commands. #450 established that which one you are in decides how fd 0
// is treated, so each is measured rather than assumed from another.
type readingShape string

const (
	shapeFile  readingShape = "script file"
	shapeRedir readingShape = "stdin redirected from the file"
	shapePipe  readingShape = "script piped in"
	shapeDashC readingShape = "-c string"
)

var allShapes = []readingShape{shapeFile, shapeRedir, shapePipe, shapeDashC}

// runShape runs body through shell in the given shape, from dir, and
// returns the combined output and exit status.
//
// Standard input is /dev/null for the shapes which do not read the
// script from it: what a script's fd 0 holds must not decide what
// `. /dev/stdin` inside a pipeline reads, and the pre-fix koi's answer
// came from exactly there.
func runShape(t *testing.T, dir, shell string, shape readingShape, body string) (string, int) {
	t.Helper()
	path := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	switch shape {
	case shapeFile:
		cmd = exec.CommandContext(t.Context(), shell, "./script.sh")
	case shapeDashC:
		cmd = exec.CommandContext(t.Context(), shell, "-c", body)
	case shapeRedir, shapePipe:
		cmd = exec.CommandContext(t.Context(), shell)
	}
	switch shape {
	case shapeRedir:
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cmd.Stdin = f // a real file on fd 0, the `koi < script` shape
	case shapePipe:
		// An io.Reader which is not a file makes os/exec build a pipe,
		// which is the `cat script | koi` shape.
		cmd.Stdin = strings.NewReader(body)
	default:
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer devnull.Close()
		cmd.Stdin = devnull
	}
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"LC_ALL=C", "TERM=dumb", "KOI_WELCOME=off",
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	case err != nil:
		t.Fatalf("running %s: %v", shell, err)
	}
	return string(out), 0
}

// devFDCase is a script measured against bash in every reading shape.
type devFDCase struct {
	name string
	body string
	// want is a line the *oracle's* output must contain, so a case
	// cannot pass by both shells saying nothing.
	want string
}

var devFDCases = []devFDCase{
	{
		// The issue's own three lines. The trailing echoes are not
		// decoration: a fix which read the pipe and then stopped reading
		// the script would satisfy `sourced` alone.
		name: "source dev stdin in a pipeline",
		body: "echo one\n" +
			"echo \"echo sourced\" | . /dev/stdin\n" +
			"echo two\necho three\n",
		want: "sourced",
	},
	{
		name: "dev fd zero is the same descriptor",
		body: "echo one\necho \"echo sourced\" | . /dev/fd/0\necho two\n",
		want: "sourced",
	},
	{
		name: "spelled source",
		body: "echo one\necho \"echo sourced\" | source /dev/stdin\necho two\n",
		want: "sourced",
	},
	{
		name: "several lines from the pipe",
		body: "echo one\nprintf 'echo A\\necho B\\necho C\\n' | . /dev/stdin\necho two\n",
		want: "\nC\n",
	},
	{
		// An empty pipe sources nothing and succeeds, which is the shape
		// the broken version accidentally produced for everything.
		name: "an empty pipe",
		body: "echo one\nprintf '' | . /dev/stdin\necho \"status=$?\"\necho two\n",
		want: "status=0",
	},
	{
		name: "the sourced text's own status",
		body: "echo one\necho false | . /dev/stdin\necho \"status=$?\"\necho two\n",
		want: "status=1",
	},
	{
		name: "inside a function",
		body: "f() { echo \"echo infunc\" | . /dev/stdin; }\necho one\nf\necho two\n",
		want: "infunc",
	},
	{
		name: "inside a sourced file",
		body: "printf 'echo \"echo fromlib\" | . /dev/stdin\\n' > lib.sh\n" +
			"echo one\n. ./lib.sh\necho two\n",
		want: "fromlib",
	},
	{
		// The sourced text may itself source the pipe it came from.
		name: "nested dev stdin",
		body: "echo one\n" +
			"printf 'echo inner; echo \"echo deeper\" | . /dev/stdin\\n' | . /dev/stdin\n" +
			"echo two\n",
		want: "deeper",
	},
	{
		// Process substitution always worked, which was the clue that
		// the machinery for a non-seekable source was there.
		name: "process substitution still works",
		body: "echo one\n. <(echo \"echo two\")\necho three\n",
		want: "two",
	},
	{
		// Non-seekable like a pipe and a different path into the same
		// code; source6.sub tests one right after the /dev/stdin block.
		name: "a fifo",
		body: "echo one\nmkfifo ./f\n{ echo \"echo fromfifo\"; } > ./f &\n. ./f\nwait\necho two\n",
		want: "fromfifo",
	},
	{
		name: "a descriptor the script opened itself",
		body: "echo one\nprintf 'echo fromfd3\\n' > three.txt\nexec 3< ./three.txt\n" +
			". /dev/fd/3\nexec 3<&-\necho two\n",
		want: "fromfd3",
	},
	{
		// Re-opening standard input by name is a no-op in bash, and it
		// is the case which pins the descriptor being handed on as the
		// file it is rather than as a copy of it: a copying goroutine
		// would swallow the rest of a script the shell is reading from
		// that very descriptor.
		name: "re-opening standard input by name",
		body: "echo one\nexec 0< /dev/stdin\necho two\necho three\n",
		want: "three",
	},
	{
		// A descriptor opened from another one by name is still a real
		// file, which is what a child process needs: koi hands its table
		// to a child as files, and a wrapper standing in for one would
		// arrive as a closed descriptor.
		name: "a descriptor opened from another, then handed to a child",
		body: "printf 'X1\\nX2\\n' > two.txt\nexec 7< ./two.txt\nexec 8< /dev/fd/7\n" +
			"cat /dev/fd/8\necho \"status=$?\"\ncat <&8\necho done\n",
		want: "X2",
	},
	{
		// The same cause on the writing side: the shell's fd 1 inside a
		// redirected group is not the process's.
		name: "writing to dev stdout inside a redirection",
		body: "echo one\n{ echo A > /dev/stdout; } > inner.txt\n" +
			"echo \"inner=[$(cat inner.txt)]\"\necho two\n",
		want: "inner=[A]",
	},
	{
		name: "a redirection reading dev stdin in a pipeline",
		body: "echo one\necho hi | { read v < /dev/stdin; echo \"got=$v\"; }\necho two\n",
		want: "got=hi",
	},
}

func TestShellDescriptorPathsMatchBash(t *testing.T) {
	if testing.Short() {
		t.Skip("differential /dev/fd behaviour skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	for _, tc := range devFDCases {
		for _, shape := range allShapes {
			t.Run(tc.name+" ("+string(shape)+")", func(t *testing.T) {
				bashOut, bashCode := runShape(t, t.TempDir(), bash, shape, tc.body)
				koiOut, koiCode := runShape(t, t.TempDir(), koi, shape, tc.body)
				if !strings.Contains(bashOut, tc.want) {
					t.Fatalf("the oracle did not produce %q, so this case cannot detect its absence: %q",
						tc.want, bashOut)
				}
				if bashOut != koiOut || bashCode != koiCode {
					t.Errorf("koi differs from bash\n  bash: %q (exit %d)\n  koi:  %q (exit %d)",
						bashOut, bashCode, koiOut, koiCode)
				}
			})
		}
	}
}

// A long tail after the pipeline, because "the rest of the script is
// gone" is the half of the bug a test asserting the sourced text is
// present cannot see. The script prints far more than one reader's
// buffer holds, so a version which consumed the script through fd 0
// loses a run of lines in the middle rather than failing outright.
func TestSourcingDevStdinDoesNotEatTheScript(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	var b strings.Builder
	for i := 1; i <= 40; i++ {
		b.WriteString("echo head" + strconv.Itoa(i) + "\n")
	}
	b.WriteString("echo \"echo sourced\" | . /dev/stdin\n")
	for i := 1; i <= 400; i++ {
		b.WriteString("echo tail" + strconv.Itoa(i) + "\n")
	}
	body := b.String()

	for _, shape := range allShapes {
		t.Run(string(shape), func(t *testing.T) {
			bashOut, bashCode := runShape(t, t.TempDir(), bash, shape, body)
			koiOut, koiCode := runShape(t, t.TempDir(), koi, shape, body)
			if !strings.Contains(bashOut, "\ntail400\n") {
				t.Fatalf("the oracle stopped early itself: %q", lastLines(bashOut))
			}
			if bashOut != koiOut || bashCode != koiCode {
				t.Errorf("koi differs from bash (koi ends %q, exit %d; bash exit %d)",
					lastLines(koiOut), koiCode, bashCode)
			}
		})
	}
}

// A descriptor which is closed, or open the other way, is an error
// rather than an empty file — the difference between a script which says
// so and one which silently sources nothing. The statuses are compared
// against bash; the wording is asserted, since bash prefixes its own
// `$0: line N:` where koi prints neither for a command string (#120,
// #571).
func TestShellDescriptorPathsRefuseWhatBashRefuses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short")
	}
	koi := buildKoi(t)
	bash := requireBash(t)

	cases := []struct {
		name string
		body string
		want string // a substring of koi's own diagnostic
	}{
		{
			name: "sourcing a closed descriptor",
			body: "exec 0<&-\n. /dev/stdin\necho \"status=$?\"\n",
			want: "/dev/stdin: bad file descriptor",
		},
		{
			name: "sourcing a descriptor open for writing",
			body: "exec 9> out.txt\n. /dev/fd/9\necho \"status=$?\"\n",
			want: "/dev/fd/9: Permission denied",
		},
		{
			name: "writing to a descriptor open for reading",
			body: "exec 8< /dev/null\necho x > /dev/fd/8\necho \"status=$?\"\n",
			want: "/dev/fd/8: permission denied",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bashOut, _ := runShape(t, t.TempDir(), bash, shapeFile, tc.body)
			koiOut, _ := runShape(t, t.TempDir(), koi, shapeFile, tc.body)
			// The status the script observed, which is what a caller
			// acts on, rather than the shell's own exit code.
			bashStatus := statusLine(bashOut)
			koiStatus := statusLine(koiOut)
			if bashStatus == "status=0" {
				t.Fatalf("the oracle accepted this, so it is not a refusal: %q", bashOut)
			}
			if bashStatus != koiStatus {
				t.Errorf("status differs from bash: bash %q, koi %q\n  bash: %q\n  koi:  %q",
					bashStatus, koiStatus, bashOut, koiOut)
			}
			if !strings.Contains(koiOut, tc.want) {
				t.Errorf("koi said nothing about the refusal: want %q in %q", tc.want, koiOut)
			}
		})
	}
}

func statusLine(out string) string {
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "status=") {
			return line
		}
	}
	return ""
}

func lastLines(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.Join(lines, "\n")
}
