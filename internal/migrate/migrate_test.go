package migrate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/gish/internal/migrate"
)

// The import is only trustworthy if it is complete *or* honest, so the
// tests are about both: what came across, and what was reported as not
// coming across.

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDetectReadsAnOrdinarySetup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HISTFILE", "") // never reach for the developer's own history
	write(t, home, ".zshrc", `
export EDITOR=nvim
PATH=$HOME/bin:/opt/tools/bin:$PATH
alias ll='ls -alF'
alias gs='git status'
mkcd() { mkdir -p "$1" && cd "$1"; }
ZSH_THEME="agnoster"
eval "$(starship init zsh)"
setopt AUTO_CD
`)

	plan, err := migrate.Detect(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Aliases) != 2 {
		t.Errorf("aliases = %+v, want 2", plan.Aliases)
	}
	if len(plan.Functions) != 1 || plan.Functions[0] != "mkcd" {
		t.Errorf("functions = %v, want [mkcd]", plan.Functions)
	}
	if len(plan.Exports) != 1 || plan.Exports[0].Name != "EDITOR" {
		t.Errorf("exports = %+v, want EDITOR", plan.Exports)
	}
	if len(plan.PathAdds) != 2 {
		t.Errorf("PATH adds = %v, want two", plan.PathAdds)
	}
	// The rc sets an oh-my-zsh theme and *then* runs starship: the
	// second one is the prompt the user actually sees.
	if plan.Theme != "starship" {
		t.Errorf("theme = %q, want starship (the last one set wins, as in the rc)", plan.Theme)
	}
	// A command that does not translate must be named, not dropped.
	if !strings.Contains(plan.Report(), "setopt AUTO_CD") {
		t.Errorf("setopt was dropped silently:\n%s", plan.Report())
	}
}

// Aliases inside a conditional stay there: the condition was written
// for a reason, usually "only on this machine".
func TestConditionalsAreReportedNotFlattened(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HISTFILE", "")
	write(t, home, ".bashrc", `
alias always='echo yes'
if [ "$(uname)" = Linux ]; then
  alias onlylinux='echo penguin'
fi
`)
	plan, err := migrate.Detect(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Aliases {
		if a.Name == "onlylinux" {
			t.Error("an alias inside a conditional was imported unconditionally")
		}
	}
	if !strings.Contains(plan.Report(), "control flow") {
		t.Errorf("the conditional was not reported:\n%s", plan.Report())
	}
}

// A zsh rc with zsh-only syntax must not cost the whole file.
func TestZshOnlySyntaxFallsBackToLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HISTFILE", "")
	write(t, home, ".zshrc", `
alias ll='ls -l'
autoload -Uz compinit && compinit
zstyle ':completion:*' menu select
foo() { print -l ${(f)"$(ls)"} }
export EDITOR=vim
`)
	plan, err := migrate.Detect(home)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, a := range plan.Aliases {
		got[a.Name] = a.Value
	}
	if got["ll"] != "ls -l" {
		t.Errorf("the alias before the zsh-only line was lost: %+v", plan.Aliases)
	}
	found := false
	for _, e := range plan.Exports {
		if e.Name == "EDITOR" {
			found = true
		}
	}
	if !found {
		t.Errorf("the export after the zsh-only line was lost: %+v", plan.Exports)
	}
}

func TestParseHistoryBothFormats(t *testing.T) {
	t.Parallel()

	t.Run("zsh extended", func(t *testing.T) {
		entries := migrate.ParseHistory(": 1700000000:0;echo hello\n: 1700000060:2;make build\n")
		if len(entries) != 2 {
			t.Fatalf("entries = %+v", entries)
		}
		if entries[0].Command != "echo hello" || entries[0].UnixSec != 1700000000 {
			t.Errorf("first = %+v", entries[0])
		}
		// zsh records elapsed seconds; the store keeps milliseconds.
		if entries[1].DurationMs != 2000 {
			t.Errorf("duration = %d ms, want 2000", entries[1].DurationMs)
		}
	})

	t.Run("zsh multi-line", func(t *testing.T) {
		entries := migrate.ParseHistory(": 1700000000:0;git commit -m \"one\\\n two\"\n")
		if len(entries) != 1 {
			t.Fatalf("a continued entry was split: %+v", entries)
		}
		if !strings.Contains(entries[0].Command, "\n") {
			t.Errorf("the continuation was lost: %q", entries[0].Command)
		}
	})

	t.Run("bash plain and timestamped", func(t *testing.T) {
		entries := migrate.ParseHistory("ls -l\n#1700000000\nmake test\n")
		if len(entries) != 2 {
			t.Fatalf("entries = %+v", entries)
		}
		if entries[0].Command != "ls -l" || entries[0].UnixSec != 0 {
			t.Errorf("plain entry = %+v", entries[0])
		}
		if entries[1].Command != "make test" || entries[1].UnixSec != 1700000000 {
			t.Errorf("timestamped entry = %+v", entries[1])
		}
	})

	t.Run("a zsh metadata line is never a command", func(t *testing.T) {
		// Reading a zsh file as bash imports `: 1700000000:0;ls` as a
		// command, which is the failure that makes an import look
		// haunted three weeks later.
		for _, e := range migrate.ParseHistory(": 1700000000:0;ls\n") {
			if strings.HasPrefix(e.Command, ": 17") {
				t.Errorf("metadata imported as a command: %q", e.Command)
			}
		}
	})
}

// The generated rc has to be a file gish can actually read.
func TestGeneratedRCParses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HISTFILE", "")
	write(t, home, ".bashrc", `
alias q="it's quoted"
export GREETING="hello world"
PATH=/opt/bin:$PATH
helper() { echo "$@"; }
`)
	plan, err := migrate.Detect(home)
	if err != nil {
		t.Fatal(err)
	}
	rc := plan.GishRC()
	for _, want := range []string{"alias q=", "export GREETING=", "PATH=/opt/bin:$PATH", "helper()"} {
		if !strings.Contains(rc, want) {
			t.Errorf("rc is missing %q:\n%s", want, rc)
		}
	}
	// An apostrophe inside an alias is the classic way a generated rc
	// stops parsing.
	if strings.Contains(rc, "alias q='it's quoted'") {
		t.Errorf("the apostrophe was not quoted:\n%s", rc)
	}
}
