package repl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/expand"
)

// The comparison decides whether a user is nagged about nothing, so
// every ambiguity has to resolve toward silence.
func TestNewerVersionResolvesAmbiguityTowardSilence(t *testing.T) {
	louder := []struct{ cur, latest string }{
		{"0.1.0", "0.2.0"},
		{"0.1.9", "0.1.10"}, // not a string comparison
		{"1.2.3", "2.0.0"},
		{"v0.1.0", "v0.1.1"}, // the tap writes bare, tags write v-prefixed
		{"0.1", "0.2"},
	}
	for _, c := range louder {
		if !newerVersion(c.cur, c.latest) {
			t.Errorf("newerVersion(%q, %q) = false, want true", c.cur, c.latest)
		}
	}

	silent := []struct {
		cur, latest, why string
	}{
		{"0.2.0", "0.1.0", "older"},
		{"0.1.0", "0.1.0", "identical"},
		{"dev", "9.9.9", "a source build is not a release and must never be nagged"},
		{"", "1.0.0", "no version stamped"},
		{"0.1.0", "", "nothing local knew"},
		{"0.1.0", "not-a-version", "unparsable metadata must not become an update"},
		{"0.1.0", "0.1.0.1", "four components is not a shape we understand"},
		{"0.1.0", "0.1.1-rc1", "a pre-release is not an upgrade to offer"},
	}
	for _, c := range silent {
		if newerVersion(c.cur, c.latest) {
			t.Errorf("newerVersion(%q, %q) = true, want false (%s)", c.cur, c.latest, c.why)
		}
	}
}

// A pre-release must not be offered, but the *stable* it precedes must
// still be seen once it lands — this pins that the suffix is trimmed for
// comparison rather than the whole version being rejected.
func TestPreReleaseSuffixIsTrimmedNotFatal(t *testing.T) {
	if !newerVersion("0.1.0-rc1", "0.2.0") {
		t.Error("a user running a pre-release was never told about the next stable")
	}
}

// The notice appears once and then never again in the session: a shell
// that repeats it every prompt is the thing people disable.
func TestNoticeShowsOnceAndHonorsTheSetting(t *testing.T) {
	n := &updateNotifier{}
	n.latest.Store("9.9.9")
	old := Version
	Version = "0.1.0"
	t.Cleanup(func() { Version = old })

	runner := newTestRunner(t)
	var first bytes.Buffer
	n.atPrompt(runner, &first)
	if !strings.Contains(first.String(), "9.9.9") {
		t.Fatalf("no notice on the first prompt: %q", first.String())
	}
	if !strings.Contains(first.String(), "0.1.0") {
		t.Error("the notice does not say which version is running")
	}

	var second bytes.Buffer
	n.atPrompt(runner, &second)
	if second.Len() != 0 {
		t.Errorf("notice repeated at the next prompt: %q", second.String())
	}
}

// Nothing local knowing anything is the normal case for a source build,
// and it must be silent rather than speculative.
func TestNoticeIsSilentWithoutLocalMetadata(t *testing.T) {
	n := &updateNotifier{}
	var out bytes.Buffer
	n.atPrompt(newTestRunner(t), &out)
	if out.Len() != 0 {
		t.Errorf("spoke with nothing to say: %q", out.String())
	}
}

// The tap formula is the local file that answers, and reading it is the
// whole mechanism: if this stops matching Homebrew's format the feature
// silently stops working, so the shape is pinned.
func TestTapFormulaVersionIsRead(t *testing.T) {
	dir := t.TempDir()
	formula := filepath.Join(dir, "Library", "Taps", "blairham", "homebrew-tap", "Formula", "koi.rb")
	if err := os.MkdirAll(filepath.Dir(formula), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "class Koi < Formula\n  desc \"shell\"\n  version \"1.4.2\"\n  url \"https://example.invalid/koi.tar.gz\"\nend\n"
	if err := os.WriteFile(formula, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := matchFile(formula, tapVersionRe); got != "1.4.2" {
		t.Errorf("read %q from the tap formula, want 1.4.2", got)
	}
}

// Homebrew's cached core metadata is one long JSON line; only the stable
// version is wanted out of it.
func TestCoreAPICacheVersionIsRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "koi.json")
	body := `{"name":"koi","desc":"shell","versions":{"stable":"2.0.1","head":"HEAD","bottle":true}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := matchFile(path, coreVersionRe); got != "2.0.1" {
		t.Errorf("read %q from the cached formula JSON, want 2.0.1", got)
	}
}

// The feature's central promise: no network, no subprocess. There is no
// way to assert "did not open a socket" from here, so this asserts the
// next best thing — the lookup consults only paths under roots it was
// given, and answers nothing when they are absent.
func TestLookupIsLocalOnly(t *testing.T) {
	t.Setenv("HOMEBREW_CACHE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if v, src := localLatestVersion(); v != "" {
		t.Errorf("invented a version %q from %q with no local metadata present", v, src)
	}
}

// `config update.notify off` has to actually silence it — the setting is
// the only reason a user who does not want the line has to keep using
// the shell rather than patching it out.
func TestNoticeRespectsTheOffSetting(t *testing.T) {
	n := &updateNotifier{}
	n.latest.Store("9.9.9")
	old := Version
	Version = "0.1.0"
	t.Cleanup(func() { Version = old })

	runner := newTestRunner(t)
	runner.Env = expand.ListEnviron("KOI_UPDATE_NOTIFY=off")

	var out bytes.Buffer
	n.atPrompt(runner, &out)
	if out.Len() != 0 {
		t.Errorf("notice printed with update.notify off: %q", out.String())
	}
}
