package repl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// koi adopt (#209). Every test isolates XDG and HOME — an adoption is
// real user state in the same way a home directory is.

func adoptTestEnv(t *testing.T) (repo string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("KOI_RC", filepath.Join(base, "home", "koirc-none"))
	repo = filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	return repo
}

func writeTeamConfig(t *testing.T, repo, content string) string {
	t.Helper()
	path := filepath.Join(repo, ".koi.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAdoptAppliesAndReverts(t *testing.T) {
	repo := adoptTestEnv(t)
	writeTeamConfig(t, repo, `
[settings]
theme = "p10k"
editmode = "vi"

[[plugins]]
source = "zsh-users/zsh-autosuggestions"
pin = "v0.7.1"
`)
	var out, errOut strings.Builder
	if err := RunAdopt(strings.NewReader(""), &out, &errOut, []string{"--yes"}); err != nil {
		t.Fatalf("adopt: %v\n%s", err, errOut.String())
	}

	// The fragment carries the settings as the rc lines config would
	// write, under the config home, one file for this repo.
	frags := adoptedFragments()
	if len(frags) != 1 {
		t.Fatalf("fragments = %v, want exactly one", frags)
	}
	raw, err := os.ReadFile(frags[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`export KOI_THEME="p10k"`, `export KOI_EDIT_MODE="vi"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("fragment lacks %q:\n%s", want, raw)
		}
	}

	// The plugin entry landed in the user's manifest.
	manifestRaw, _ := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "koi", "plugins.toml"))
	if !strings.Contains(string(manifestRaw), "zsh-users/zsh-autosuggestions") {
		t.Errorf("plugins.toml lacks the adopted entry:\n%s", manifestRaw)
	}

	// Revert removes both, exactly.
	out.Reset()
	if err := RunAdopt(strings.NewReader(""), &out, &errOut, []string{"--revert"}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if frags := adoptedFragments(); len(frags) != 0 {
		t.Errorf("fragments after revert = %v, want none", frags)
	}
	manifestRaw, _ = os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "koi", "plugins.toml"))
	if strings.Contains(string(manifestRaw), "zsh-users/zsh-autosuggestions") {
		t.Errorf("plugins.toml still carries the adopted entry after revert:\n%s", manifestRaw)
	}
}

// Validation is all-or-nothing against config's own vocabulary: an
// unknown name or a value outside the enumerated set applies nothing.
func TestAdoptValidatesAgainstConfigVocabulary(t *testing.T) {
	repo := adoptTestEnv(t)

	writeTeamConfig(t, repo, "[settings]\nmystery = \"on\"\n")
	var out, errOut strings.Builder
	if err := RunAdopt(strings.NewReader(""), &out, &errOut, []string{"--yes"}); err == nil {
		t.Fatal("an unknown setting name was adopted")
	}

	writeTeamConfig(t, repo, "[settings]\ntheme = \"disco\"\n")
	if err := RunAdopt(strings.NewReader(""), &out, &errOut, []string{"--yes"}); err == nil {
		t.Fatal("a value outside the enumerated set was adopted")
	}
	if frags := adoptedFragments(); len(frags) != 0 {
		t.Errorf("rejected configs still wrote fragments: %v", frags)
	}
}

// Without --yes the preview asks, and anything but yes applies nothing.
func TestAdoptDeclineAppliesNothing(t *testing.T) {
	repo := adoptTestEnv(t)
	writeTeamConfig(t, repo, "[settings]\ntheme = \"p10k\"\n")
	var out, errOut strings.Builder
	if err := RunAdopt(strings.NewReader("n\n"), &out, &errOut, nil); err != nil {
		t.Fatal(err)
	}
	if frags := adoptedFragments(); len(frags) != 0 {
		t.Errorf("declined adoption still wrote fragments: %v", frags)
	}
	if !strings.Contains(out.String(), "nothing applied") {
		t.Errorf("the decline was not acknowledged: %q", out.String())
	}
}

// An unpinned plugin entry warns — the team's config would drift per
// machine, which is what pins exist to stop — but does not block.
func TestAdoptWarnsOnUnpinnedPlugin(t *testing.T) {
	repo := adoptTestEnv(t)
	writeTeamConfig(t, repo, "[[plugins]]\nsource = \"user/tool\"\n")
	var out, errOut strings.Builder
	if err := RunAdopt(strings.NewReader(""), &out, &errOut, []string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "unpinned") {
		t.Errorf("no drift warning for an unpinned entry: %q", errOut.String())
	}
}

// The fragment runs before the user's rc, so the user's own setting
// wins by source order — the layering rule made mechanical.
func TestAdoptedFragmentRunsUnderTheUserRC(t *testing.T) {
	repo := adoptTestEnv(t)
	writeTeamConfig(t, repo, "[settings]\ntheme = \"p10k\"\n")
	var out, errOut strings.Builder
	if err := RunAdopt(strings.NewReader(""), &out, &errOut, []string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(t.TempDir(), "koirc")
	if err := os.WriteFile(rc, []byte("KOI_THEME=koi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOI_RC", rc)

	runner := newTestRunner(t)
	loadRC(context.Background(), runner)
	if got := shellVar(runner, "KOI_THEME", ""); got != "koi" {
		t.Errorf("KOI_THEME = %q — the user's rc must beat the adopted fragment", got)
	}

	// And with no user setting, the fragment's value stands.
	if err := os.WriteFile(rc, []byte("true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner = newTestRunner(t)
	loadRC(context.Background(), runner)
	if got := shellVar(runner, "KOI_THEME", ""); got != "p10k" {
		t.Errorf("KOI_THEME = %q — the adopted fragment did not apply", got)
	}
}

// Revert leaves alone a plugin entry the user has edited since: it is
// theirs now, and deleting it would revert more than the adoption.
func TestRevertSkipsEditedPluginEntries(t *testing.T) {
	repo := adoptTestEnv(t)
	writeTeamConfig(t, repo, "[[plugins]]\nsource = \"user/tool\"\npin = \"v1\"\n")
	var out, errOut strings.Builder
	if err := RunAdopt(strings.NewReader(""), &out, &errOut, []string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	// The user repins by hand.
	mpath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "koi", "plugins.toml")
	raw, _ := os.ReadFile(mpath)
	edited := strings.ReplaceAll(string(raw), `"v1"`, `"v2"`)
	if err := os.WriteFile(mpath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := RunAdopt(strings.NewReader(""), &out, &errOut, []string{"--revert"}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(mpath)
	if !strings.Contains(string(raw), "user/tool") {
		t.Error("revert removed an entry the user had edited")
	}
}

// doctor's staleness half: adopted-and-current is quiet, a changed file
// warns with the re-apply/revert choice, and a repo the user chose not
// to adopt is never nagged about.
func TestCheckAdoptedNoticesDrift(t *testing.T) {
	repo := adoptTestEnv(t)
	writeTeamConfig(t, repo, "[settings]\ntheme = \"p10k\"\n")

	if r := checkAdopted(); r.status != checkOK || !strings.Contains(r.detail, "not adopted") {
		t.Errorf("unadopted config: %+v", r)
	}

	var out, errOut strings.Builder
	if err := RunAdopt(strings.NewReader(""), &out, &errOut, []string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	if r := checkAdopted(); r.status != checkOK || !strings.Contains(r.detail, "current") {
		t.Errorf("freshly adopted: %+v", r)
	}

	writeTeamConfig(t, repo, "[settings]\ntheme = \"starship\"\n")
	if r := checkAdopted(); r.status != checkWarn {
		t.Errorf("a changed team config did not warn: %+v", r)
	}
}
