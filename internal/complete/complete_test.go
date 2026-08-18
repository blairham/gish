package complete_test

import (
	"os"
	"path/filepath"
	"runtime"
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
