package complete_test

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/blairham/koi-shell/internal/complete"
)

func TestAnalyze(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text      string
		cursor    int
		word      string
		start     int
		isCommand bool
	}{
		{"gi", 2, "gi", 0, true},
		{"git sta", 7, "sta", 4, false},
		{"echo a; ma", 10, "ma", 8, true},
		{"ls | gr", 7, "gr", 5, true},
		{"cat ./RE", 8, "./RE", 4, false},
		{"", 0, "", 0, true},
		{"echo 'sp", 8, "sp", 5, false},
	}
	for _, tt := range tests {
		word, start, isCmd := complete.Analyze(tt.text, tt.cursor)
		if word != tt.word || start != tt.start || isCmd != tt.isCommand {
			t.Errorf("Analyze(%q,%d) = (%q,%d,%v), want (%q,%d,%v)",
				tt.text, tt.cursor, word, start, isCmd, tt.word, tt.start, tt.isCommand)
		}
	}
}

func TestFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{"readme.md", "release.txt", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "rel-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := complete.Files("re", dir)
	want := []string{"readme.md", "rel-dir/", "release.txt"}
	if len(got) != len(want) {
		t.Fatalf("Files = %+v, want %v", got, want)
	}
	for i, w := range want {
		if got[i].Value != w {
			t.Errorf("Files[%d] = %q, want %q", i, got[i].Value, w)
		}
	}

	// Hidden entries only with a dotted prefix.
	if got := complete.Files("", dir); len(got) != 3 {
		t.Errorf("unprefixed Files included hidden: %+v", got)
	}
	if got := complete.Files(".h", dir); len(got) != 1 || got[0].Value != ".hidden" {
		t.Errorf("dotted prefix = %+v", got)
	}

	// Subdirectory paths keep their dir part in the value.
	got = complete.Files("rel-dir/", dir)
	if len(got) != 0 {
		t.Errorf("empty dir = %+v", got)
	}
}

func TestCommands(t *testing.T) {
	t.Parallel()

	bin := t.TempDir()
	// An executable on this platform: exec bit on unix, .exe on Windows
	// (where completion strips the extension back to "mycmd").
	exeName := "mycmd"
	if runtime.GOOS == "windows" {
		exeName = "mycmd.exe"
	}
	if err := os.WriteFile(filepath.Join(bin, exeName), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "not-exec"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := complete.Commands("my", bin, []string{"myfunc", "other"})
	values := map[string]bool{}
	for _, c := range got {
		values[c.Value] = true
	}
	if !values["mycmd"] || !values["myfunc"] {
		t.Errorf("Commands = %+v", got)
	}
	if values["not-exec"] || values["other"] {
		t.Errorf("Commands included non-matches: %+v", got)
	}
}

// The editor's listings are sorted here, and that is load-bearing
// elsewhere: `compgen` deliberately does not sort (#613, bash keeps
// generation order), so the completion *menu*'s order has to come from
// this package rather than from the shared sort compgen used to apply.
// Nothing asserted it, which is how a deletion here would have quietly
// unsorted the menu while every compgen case still passed.
//
// The fixture is created in an unsorted order, so a listing that simply
// forwarded readdir would fail: os.ReadDir happens to sort too, which
// would make this vacuous on its own — hence Commands, whose input is a
// map and whose order can only come from the sort.
func TestListingsAreSortedForTheEditor(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zeta", "alpha", "mu"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"zdir", "adir"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	var entries []string
	for _, e := range complete.Entries("", dir, true) {
		entries = append(entries, e.Value)
	}
	if !slices.IsSorted(entries) {
		t.Errorf("Entries is not sorted: %q", entries)
	}

	var files []string
	for _, c := range complete.Files("", dir) {
		files = append(files, c.Value)
	}
	if !slices.IsSorted(files) {
		t.Errorf("Files is not sorted: %q", files)
	}

	bin := t.TempDir()
	for _, name := range []string{"zcmd", "acmd", "mcmd"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // an executable fixture
			t.Fatal(err)
		}
	}
	var cmds []string
	for _, c := range complete.Commands("", bin, []string{"zfunc", "afunc"}) {
		cmds = append(cmds, c.Value)
	}
	if len(cmds) == 0 {
		t.Fatal("Commands listed nothing, so sortedness proves nothing")
	}
	if !slices.IsSorted(cmds) {
		t.Errorf("Commands is not sorted: %q", cmds)
	}
}
