package emit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/gish/internal/plugmgr/ice"
	"github.com/blairham/gish/internal/plugmgr/spec"
)

func mkfiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		path := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# stub\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveSourceFilePrefersRepoNamedPluginFile(t *testing.T) {
	dir := t.TempDir()
	mkfiles(t, dir, "extra.zsh", "myplugin.plugin.zsh", "README.md")
	got, err := ResolveSourceFile(dir, "myplugin", ice.New())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "myplugin.plugin.zsh" {
		t.Errorf("resolved %q, want myplugin.plugin.zsh", got)
	}
}

func TestResolveSourceFileFallbackOrder(t *testing.T) {
	dir := t.TempDir()
	mkfiles(t, dir, "helper.sh", "theme.zsh-theme")
	got, err := ResolveSourceFile(dir, "other", ice.New())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "theme.zsh-theme" {
		t.Errorf("resolved %q, want theme.zsh-theme (themes beat plain .sh)", got)
	}
}

func TestResolveSourceFilePickWins(t *testing.T) {
	dir := t.TempDir()
	mkfiles(t, dir, "a.plugin.zsh", "special/init.zsh")
	ic := ice.New()
	ic.Set("pick", "special/init.zsh")
	got, err := ResolveSourceFile(dir, "a", ic)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "special/init.zsh") {
		t.Errorf("resolved %q, want special/init.zsh", got)
	}
}

func TestResolveSourceFileEmptyDirErrors(t *testing.T) {
	if _, err := ResolveSourceFile(t.TempDir(), "x", ice.New()); err == nil {
		t.Error("want error for empty dir, got nil")
	}
}

func TestPluginPayload(t *testing.T) {
	s, err := spec.ParsePlugin("user/repo", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ic, err := ice.ParseArgs([]string{"atinitecho init", "atloadecho loaded", "wait1"})
	if err != nil {
		t.Fatal(err)
	}
	payload := PluginPayload(s, "/tmp/p", "/tmp/p/repo.plugin.zsh", ic)
	for _, want := range []string{
		"fpath=( '/tmp/p' $fpath )",
		"echo init",
		"builtin source '/tmp/p/repo.plugin.zsh'",
		"echo loaded",
		"ZI_GO_LOADED+=( 'user/repo' )",
		"Loaded %s", // turbo without lucid announces itself (printf form)
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %q:\n%s", want, payload)
		}
	}
	// atinit must precede source, atload must follow it.
	if strings.Index(payload, "echo init") > strings.Index(payload, "builtin source") {
		t.Error("atinit should run before source")
	}
	if strings.Index(payload, "echo loaded") < strings.Index(payload, "builtin source") {
		t.Error("atload should run after source")
	}
}

func TestPluginPayloadAsProgram(t *testing.T) {
	s, _ := spec.ParsePlugin("junegunn/fzf", "", "")
	ic, _ := ice.ParseArgs([]string{"asprogram"})
	payload := PluginPayload(s, "/tmp/fzf", "/tmp/fzf/fzf", ic)
	if !strings.Contains(payload, `PATH='/tmp/fzf':"$PATH"`) {
		t.Errorf("as'program' should extend PATH:\n%s", payload)
	}
	if strings.Contains(payload, "builtin source") {
		t.Errorf("as'program' should not source anything:\n%s", payload)
	}
}

func TestPayloadGuards(t *testing.T) {
	s, _ := spec.ParsePlugin("user/repo", "", "")
	ic, _ := ice.ParseArgs([]string{"hasnvim"})
	payload := PluginPayload(s, "/tmp/p", "/tmp/p/x.zsh", ic)
	if !strings.Contains(payload, "command -v nvim >/dev/null 2>&1 || return 0") {
		t.Errorf("has ice should guard the payload:\n%s", payload)
	}
}

func TestZshQuoting(t *testing.T) {
	s, _ := spec.ParsePlugin("user/repo", "", "")
	payload := PluginPayload(s, "/tmp/it's here", "/tmp/it's here/x.zsh", ice.New())
	if !strings.Contains(payload, `'/tmp/it'\''s here'`) {
		t.Errorf("single quotes in paths must be escaped:\n%s", payload)
	}
}
