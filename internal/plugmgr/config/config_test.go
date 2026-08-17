package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadHonorsZiGoHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZI_GO_HOME", home)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Home != home {
		t.Errorf("Home = %q, want %q", cfg.Home, home)
	}
	for name, got := range map[string]string{
		"plugins":     cfg.PluginsDir(),
		"snippets":    cfg.SnippetsDir(),
		"completions": cfg.CompletionsDir(),
		"run":         cfg.RunDir(),
	} {
		if want := filepath.Join(home, name); got != want {
			t.Errorf("%s dir = %q, want %q", name, got, want)
		}
	}
}

func TestLoadFallsBackToTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZI_GO_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads this on Windows

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".zi-go"); cfg.Home != want {
		t.Errorf("Home = %q, want %q", cfg.Home, want)
	}
}

// Startup creates nothing (#163): resolving the layout must not bring
// the directories into being, or a shell that never installs a plugin
// still litters the user's home.
func TestLoadCreatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZI_GO_HOME", filepath.Join(home, "zi"))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _ = cfg.PluginsDir(), cfg.SnippetsDir(), cfg.CompletionsDir(), cfg.RunDir()

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("resolving the layout created %v", entries)
	}
}

func TestEnsureCreatesAndIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "a", "b")
	got, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("Ensure returned %q, want %q", got, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	// Calling it again on an existing directory is not an error.
	if _, err := Ensure(dir); err != nil {
		t.Errorf("Ensure is not idempotent: %v", err)
	}
}

func TestEnsureFailsWhenAFileIsInTheWay(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(path); err == nil {
		t.Error("Ensure succeeded where a file already exists")
	}
}
