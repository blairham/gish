package repl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// The command stream follows fd 0 only when the script *is* fd 0 (#516).
//
// cmd/koi's differential tests cover the shape a user meets — `koi <
// script`, which is the piped loop — while this covers the same rule on
// the RunReader path, where the caller says what standard input is. The
// two arrangements are one line apart in the code and opposite in
// meaning, so both are pinned here rather than one being assumed from
// the other.
func TestRunReaderFollowsARedirectedCommandStream(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner.sh")
	if err := os.WriteFile(inner, []byte("echo from-inner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "outer.sh")
	// The path is quoted because on Windows it is full of backslashes,
	// which a shell would otherwise read as escapes — inside double
	// quotes a backslash is only special before $, `, " and itself.
	body := "echo one\nexec 0< \"" + inner + "\"\necho never\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("the script is standard input", func(t *testing.T) {
		f, err := os.Open(script)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var out bytes.Buffer
		// The same file is both the command stream and standard input,
		// which is what `koi < script` is.
		if err := RunReader(t.Context(), f, "test", interp.StdIO(f, &out, &out)); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != "one\nfrom-inner\n" {
			t.Errorf("output = %q, want one/from-inner", got)
		}
	})

	t.Run("the script is not standard input", func(t *testing.T) {
		f, err := os.Open(script)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var out bytes.Buffer
		// Standard input is something else, so redirecting fd 0 changes
		// only what the script's own commands read.
		if err := RunReader(t.Context(), f, "test", interp.StdIO(strings.NewReader(""), &out, &out)); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != "one\nnever\n" {
			t.Errorf("output = %q, want one/never", got)
		}
	})
}

// A line's statements are read together and run together, so a mode one
// of them sets reaches the next line rather than its own neighbors
// (#450). Everything else about reading incrementally rests on that.
func TestReadingIsALineAtATime(t *testing.T) {
	var out bytes.Buffer
	src := "printf a\nprintf b; printf c\nprintf d\n"
	if err := RunReader(t.Context(), strings.NewReader(src), "test",
		interp.StdIO(strings.NewReader(""), &out, &out)); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "abcd" {
		t.Errorf("output = %q, want abcd", got)
	}
}
