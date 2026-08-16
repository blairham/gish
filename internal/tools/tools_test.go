package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeInstalls builds an asdf-shaped install tree.
func fakeInstalls(t *testing.T, layout map[string][]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "installs")
	for toolVer, binDirs := range layout {
		for _, bin := range binDirs {
			if err := os.MkdirAll(filepath.Join(root, toolVer, bin), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func writePins(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writePins(t, dir, "# header\ngolang 1.26.6\nnodejs 22.1.0 20.0.0 # fallback\n\nbroken-line\n")
	pins := ParseFile(filepath.Join(dir, ".tool-versions"))
	if len(pins) != 2 {
		t.Fatalf("pins = %+v", pins)
	}
	if pins[0].Tool != "golang" || pins[0].Versions[0] != "1.26.6" {
		t.Errorf("pin 0 = %+v", pins[0])
	}
	if len(pins[1].Versions) != 2 {
		t.Errorf("fallback versions not parsed: %+v", pins[1])
	}
}

func TestResolveLayouts(t *testing.T) {
	t.Parallel()

	installs := fakeInstalls(t, map[string][]string{
		"golang/1.26.6": {"bin", "go/bin"}, // both layouts at once
		"helm/3.15.0":   {"bin"},
	})
	dir := t.TempDir()
	writePins(t, dir, "golang 1.26.6\nhelm 3.15.0\nnodejs 22.1.0\n")

	res := Resolve(dir, []string{installs})
	if res.File != filepath.Join(dir, ".tool-versions") {
		t.Errorf("file = %q", res.File)
	}
	want := []string{
		filepath.Join(installs, "golang/1.26.6/bin"),
		filepath.Join(installs, "golang/1.26.6/go/bin"),
		filepath.Join(installs, "helm/3.15.0/bin"),
	}
	if len(res.Bins) != 3 || res.Bins[0] != want[0] || res.Bins[1] != want[1] || res.Bins[2] != want[2] {
		t.Errorf("bins = %q, want %q", res.Bins, want)
	}
	if len(res.Missing) != 1 || res.Missing[0].Tool != "nodejs" {
		t.Errorf("missing = %+v", res.Missing)
	}
}

func TestResolveFallbackAndSystem(t *testing.T) {
	t.Parallel()

	installs := fakeInstalls(t, map[string][]string{"golang/1.25.0": {"bin"}})
	dir := t.TempDir()
	writePins(t, dir, "golang 1.99.0 1.25.0\npython system\n")

	res := Resolve(dir, []string{installs})
	if len(res.Bins) != 1 || !strings.Contains(res.Bins[0], "1.25.0") {
		t.Errorf("fallback version not used: %q", res.Bins)
	}
	// system satisfies the pin without PATH changes and is not missing.
	if len(res.Missing) != 0 {
		t.Errorf("system pin reported missing: %+v", res.Missing)
	}
}

func TestResolvePathPin(t *testing.T) {
	t.Parallel()

	custom := t.TempDir()
	if err := os.MkdirAll(filepath.Join(custom, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writePins(t, dir, "mytool path:"+custom+"\n")

	res := Resolve(dir, nil)
	if len(res.Bins) != 1 || res.Bins[0] != filepath.Join(custom, "bin") {
		t.Errorf("path: pin = %q", res.Bins)
	}
}

func TestFindFileWalksUp(t *testing.T) {
	t.Parallel()

	installs := fakeInstalls(t, map[string][]string{"golang/1.26.6": {"bin"}})
	root := t.TempDir()
	writePins(t, root, "golang 1.26.6\n")
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	if res := Resolve(nested, []string{installs}); res.File != filepath.Join(root, ".tool-versions") {
		t.Errorf("walk-up file = %q", res.File)
	}

	// The nearest file wins over an ancestor's.
	writePins(t, nested, "golang 1.99.0\n")
	res := Resolve(nested, []string{installs})
	if res.File != filepath.Join(nested, ".tool-versions") || len(res.Missing) != 1 {
		t.Errorf("nearest file did not win: %+v", res)
	}
}

func TestResolveNoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())        // no global fallback either
	t.Setenv("USERPROFILE", t.TempDir()) // UserHomeDir reads this on Windows
	res := Resolve(t.TempDir(), nil)
	if res.File != "" || len(res.Bins) != 0 || len(res.Missing) != 0 {
		t.Errorf("empty resolution expected: %+v", res)
	}
}

func TestSetPin(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".tool-versions")
	// Creates the file when missing.
	if err := SetPin(path, "golang", "1.26.6"); err != nil {
		t.Fatal(err)
	}
	// Replaces in place, preserving comments and other lines.
	if err := os.WriteFile(path, []byte("# pins\ngolang 1.0.0\nnodejs 22.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetPin(path, "golang", "1.26.6"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# pins\ngolang 1.26.6\nnodejs 22.0.0\n"
	if string(data) != want {
		t.Errorf("file = %q, want %q", data, want)
	}
	// Appends a new tool.
	if err := SetPin(path, "helm", "3.15.0"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.HasSuffix(string(data), "helm 3.15.0\n") {
		t.Errorf("append failed: %q", data)
	}
}

func TestInstalled(t *testing.T) {
	t.Parallel()

	asdf := fakeInstalls(t, map[string][]string{"golang/1.26.6": {"bin"}, "golang/1.25.0": {"bin"}})
	mise := fakeInstalls(t, map[string][]string{"golang/1.24.0": {"bin"}, "golang/1.26.6": {"bin"}})
	got := Installed([]string{asdf, mise}, "golang")
	want := []string{"1.24.0", "1.25.0", "1.26.6"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("installed = %q, want %q (deduplicated across roots)", got, want)
	}
}
