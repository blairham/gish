package installer

import (
	"os"
	"path/filepath"
	"testing"
)

// The mv/cp ices move files around inside a plugin's own directory.
// Neither side of `from -> to` may reach outside it: a plugin's install
// recipe is the plugin author's text, so an unbounded destination lets
// installing a plugin overwrite whatever the shell can write (#204).

func TestMoveOrCopyStaysInsideTheObjectDirectory(t *testing.T) {
	t.Parallel()

	for _, move := range []bool{true, false} {
		name := "copy"
		if move {
			name = "move"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			outside := t.TempDir()
			dir := filepath.Join(outside, "object")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			// A sentinel the ice tries to clobber.
			victim := filepath.Join(outside, "victim")
			if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
				t.Fatal(err)
			}

			err := moveOrCopy(dir, "payload -> ../victim", move)

			got, rerr := os.ReadFile(victim)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != "original" {
				t.Fatalf("an ice wrote outside the object directory (err=%v)", err)
			}
			if err == nil {
				t.Error("an escaping destination was accepted")
			}
		})
	}
}

// The ordinary case keeps working: a rename and a copy within the
// directory, including through a glob.
func TestMoveOrCopyWithinTheDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool.sh"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := moveOrCopy(dir, "*.sh -> bin/tool", false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "bin", "tool")); err != nil || string(got) != "body" {
		t.Fatalf("copy did not land: %q, %v", got, err)
	}
	// The copy leaves the source; the move must not.
	if _, err := os.Stat(filepath.Join(dir, "tool.sh")); err != nil {
		t.Errorf("copy removed its source: %v", err)
	}
	if err := moveOrCopy(dir, "tool.sh -> moved", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tool.sh")); !os.IsNotExist(err) {
		t.Errorf("move left its source behind: %v", err)
	}
}

func TestMoveOrCopyRejectsMalformedAndMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := moveOrCopy(dir, "no-arrow-here", false); err == nil {
		t.Error("an expression without -> was accepted")
	}
	if err := moveOrCopy(dir, "nothing-matches-* -> dst", false); err == nil {
		t.Error("a pattern matching no files was accepted")
	}
}
