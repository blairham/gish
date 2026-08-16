//go:build unix

package remote

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The failure matrix from #98, run against a real /bin/sh under
// t.TempDir(). The Local transport exists precisely so this is testable
// with no ssh, no network, and no remote host — and so the probe script
// is exercised as a shell script rather than as a Go string.

// harness points the candidate-directory chain at a temp tree and gives
// the scripts a hermetic environment. Nothing here touches the real
// /tmp, /dev/shm, or the runner's home.
type harness struct {
	t     *testing.T
	home  string
	local *Local
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// Only the temp tree is a candidate: a test must never be able to
	// fall through to the real /tmp.
	prev := candidateDirs
	candidateDirs = []string{`"${HOME:-}/.cache/gish"`, `"${GISH_TEST_ALT:-}/gish"`}
	t.Cleanup(func() { candidateDirs = prev })

	return &harness{
		t:    t,
		home: home,
		local: &Local{Env: []string{
			"HOME=" + home,
			"PATH=" + os.Getenv("PATH"),
			"GISH_TEST_ALT=" + filepath.Join(base, "alt"),
		}},
	}
}

// payload makes a small fake "binary" to push.
func payload(t *testing.T, content string) Payload {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-gish")
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil { //nolint:gosec // fake binary in a temp dir
		t.Fatal(err)
	}
	p, err := LocalBinary(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeFindsAnExecutableDirectory(t *testing.T) {
	h := newHarness(t)
	p := payload(t, "#!/bin/sh\nexit 0\n")

	got, err := RunProbe(context.Background(), h.local, p.Name, p.Sum, p.Size)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if want := filepath.Join(h.home, ".cache", "gish"); got.Dir != want {
		t.Errorf("dir = %q, want %q", got.Dir, want)
	}
	if got.OS == "" || got.Arch == "" {
		t.Errorf("platform not identified: %+v", got)
	}
	if got.Present {
		t.Error("reported the binary present before anything was pushed")
	}
}

// A directory being writable says nothing about whether a binary in it
// can run: /tmp mounted noexec is standard on CIS-benchmarked hosts,
// which is exactly the hardened box in the pitch. The probe must fall
// through to the next candidate rather than push somewhere useless.
func TestProbeSkipsNoexecDirectory(t *testing.T) {
	h := newHarness(t)
	// Simulate noexec by making the first candidate a *file*: it cannot
	// be mkdir'd, so the chain must move on. (A real noexec mount is not
	// something a test can arrange portably; what matters is that a
	// failed exec test does not stop the search.)
	cache := filepath.Join(h.home, ".cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "gish"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := payload(t, "x")
	got, err := RunProbe(context.Background(), h.local, p.Name, p.Sum, p.Size)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(got.Dir, "alt") {
		t.Errorf("dir = %q, want the second candidate", got.Dir)
	}
}

// Nowhere to write and nowhere to run: the whole chain fails, and the
// caller must get the sentinel that means "fall back to plain ssh".
func TestProbeFailsWhenNoCandidateWorks(t *testing.T) {
	h := newHarness(t)
	h.local.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent-gish-test", "GISH_TEST_ALT=/nonexistent-gish-test"}

	p := payload(t, "x")
	_, err := RunProbe(context.Background(), h.local, p.Name, p.Sum, p.Size)
	if !errors.Is(err, errNoExecDir) {
		t.Fatalf("err = %v, want errNoExecDir", err)
	}
}

func TestPushVerifiesAndInstalls(t *testing.T) {
	h := newHarness(t)
	p := payload(t, "#!/bin/sh\necho hello\n")

	probe, err := RunProbe(context.Background(), h.local, p.Name, p.Sum, p.Size)
	if err != nil {
		t.Fatal(err)
	}
	if err := Push(context.Background(), h.local, probe.Dir, p, probe.HashCmd); err != nil {
		t.Fatalf("push: %v", err)
	}

	landed := filepath.Join(probe.Dir, p.Name)
	info, err := os.Stat(landed)
	if err != nil {
		t.Fatalf("binary did not land: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", info.Mode().Perm())
	}
	// The .partial must be gone: a leftover is the thing that gets
	// exec'd forever after a dropped connection.
	if _, err := os.Stat(landed + ".partial"); err == nil {
		t.Error(".partial survived a successful push")
	}

	// Second visit: the probe now sees it and nothing needs pushing.
	again, err := RunProbe(context.Background(), h.local, p.Name, p.Sum, p.Size)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Present {
		t.Error("repeat visit did not recognize the cached binary")
	}
}

// A truncated transfer must be caught and must leave nothing behind.
func TestPushRejectsCorruptedTransfer(t *testing.T) {
	h := newHarness(t)
	p := payload(t, "the real contents")
	probe, err := RunProbe(context.Background(), h.local, p.Name, p.Sum, p.Size)
	if err != nil {
		t.Fatal(err)
	}

	// Claim a hash and size the delivered bytes will not match — the
	// same observable state a dropped connection produces.
	bad := p
	bad.Sum = strings.Repeat("0", 64)
	bad.Size = p.Size + 100

	err = Push(context.Background(), h.local, probe.Dir, bad, probe.HashCmd)
	if !errors.Is(err, errVerifyFail) {
		t.Fatalf("err = %v, want errVerifyFail", err)
	}
	if _, err := os.Stat(filepath.Join(probe.Dir, bad.Name+".partial")); err == nil {
		t.Error("a failed push left its .partial behind")
	}
	if _, err := os.Stat(filepath.Join(probe.Dir, bad.Name)); err == nil {
		t.Error("a failed push installed the file anyway")
	}
}

// No sha256sum and no shasum: verification falls back to size, which
// still catches the truncation that actually happens.
func TestPushWithoutHashToolFallsBackToSize(t *testing.T) {
	h := newHarness(t)
	p := payload(t, "some bytes here")
	probe, err := RunProbe(context.Background(), h.local, p.Name, p.Sum, p.Size)
	if err != nil {
		t.Fatal(err)
	}

	if err := Push(context.Background(), h.local, probe.Dir, p, ""); err != nil {
		t.Fatalf("push without hash tool: %v", err)
	}
	if _, err := os.Stat(filepath.Join(probe.Dir, p.Name)); err != nil {
		t.Errorf("binary did not land: %v", err)
	}

	wrong := p
	wrong.Size = p.Size + 1
	if err := Push(context.Background(), h.local, probe.Dir, wrong, ""); !errors.Is(err, errVerifyFail) {
		t.Errorf("size mismatch err = %v, want errVerifyFail", err)
	}
}

func TestProbeTimeoutIsBounded(t *testing.T) {
	h := newHarness(t)
	h.local.Shell = "/bin/sh"
	// A transport that never answers. The probe must give up on its own
	// deadline, because the whole feature is void if it makes getting a
	// shell slower than plain ssh.
	slow := &Local{Env: h.local.Env}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := runProbeScript(ctx, slow, "sleep 30")
	if err == nil {
		t.Fatal("a hung probe returned success")
	}
	if elapsed := time.Since(start); elapsed > ProbeTimeout+time.Second {
		t.Errorf("probe took %s, want it bounded near %s", elapsed, ProbeTimeout)
	}
}

// runProbeScript exercises the timeout path with an arbitrary script.
func runProbeScript(ctx context.Context, t Transport, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	return t.Run(ctx, script, nil)
}

func TestUninstallRemovesEverything(t *testing.T) {
	h := newHarness(t)
	p := payload(t, "binary")
	probe, err := RunProbe(context.Background(), h.local, p.Name, p.Sum, p.Size)
	if err != nil {
		t.Fatal(err)
	}
	if err := Push(context.Background(), h.local, probe.Dir, p, probe.HashCmd); err != nil {
		t.Fatal(err)
	}

	removed, err := Uninstall(context.Background(), h.local)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(removed) == 0 {
		t.Fatal("uninstall reported nothing removed")
	}
	if _, err := os.Stat(probe.Dir); err == nil {
		t.Errorf("%s survived uninstall", probe.Dir)
	}
}

func TestNormalizeArch(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64":  "amd64",
		"aarch64": "arm64",
		"arm64":   "arm64",
		"armv7l":  "arm",
		"riscv64": "riscv64",
		"weird":   "weird",
	} {
		if got := normalizeArch(in); got != want {
			t.Errorf("normalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
}

// The bundle carries settings and only settings. This is the test that
// keeps a future contributor from adding "just the history file" to it.
func TestBundleCarriesSettingsAndNothingElse(t *testing.T) {
	env := map[string]string{
		"GISH_THEME":            "p10k",
		"GISH_THEME_COLOR_DIR":  "cyan",
		"GISH_LINT":             "native",
		"AWS_SECRET_ACCESS_KEY": "shouldnottravel",
		"GITHUB_TOKEN":          "shouldnottravel",
		"GISH_PLUGIN_DIR":       "/local/only",
		"HOME":                  "/home/local",
	}
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	out := string(Bundle(func(k string) string { return env[k] }, names))

	for _, want := range []string{"GISH_THEME='p10k'", "GISH_THEME_COLOR_DIR='cyan'", "GISH_LINT='native'"} {
		if !strings.Contains(out, want) {
			t.Errorf("bundle missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"shouldnottravel", "GISH_PLUGIN_DIR", "HOME="} {
		if strings.Contains(out, forbidden) {
			t.Errorf("bundle carried %q, which must stay local:\n%s", forbidden, out)
		}
	}
}

func TestBundleQuotesValues(t *testing.T) {
	env := map[string]string{"GISH_PROMPT": "it's %~ $ "}
	out := string(Bundle(func(k string) string { return env[k] }, nil))
	if !strings.Contains(out, `'it'\''s %~ $ '`) {
		t.Errorf("prompt not shell-quoted:\n%s", out)
	}
}

// The remote command line is a contract with the far side: exec so no
// shell parent lingers, the rc passed as a path (never as contents,
// since argv is world-readable through /proc), and --remote-session so
// the shell knows it was brought rather than installed.
func TestRemoteCommand(t *testing.T) {
	got := remoteCommand("/home/u/.cache/gish", "gish-abc", "rc-def", false)
	for _, want := range []string{"exec ", "gish-abc", "--remote-session", "--rc ", "rc-def"} {
		if !strings.Contains(got, want) {
			t.Errorf("command %q missing %q", got, want)
		}
	}

	eph := remoteCommand("/home/u/.cache/gish", "gish-abc", "rc-def", true)
	if !strings.Contains(eph, "trap ") || !strings.Contains(eph, "rm -rf") {
		t.Errorf("ephemeral command does not clean up: %q", eph)
	}
	if strings.HasPrefix(eph, "exec ") {
		t.Error("ephemeral command exec'd, so nothing survives to clean up")
	}
}

func TestDecideRemembersPerHost(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // UserHomeDir reads this on Windows

	asked := 0
	yes := func(string) (bool, error) { asked++; return true, nil }

	if bring, _, err := Decide("host-a", BringAsk, true, yes); err != nil || !bring {
		t.Fatalf("first visit: bring=%v err=%v", bring, err)
	}
	if bring, remembered, err := Decide("host-a", BringAsk, true, yes); err != nil || !bring || !remembered {
		t.Fatalf("second visit: bring=%v remembered=%v err=%v", bring, remembered, err)
	}
	if asked != 1 {
		t.Errorf("asked %d times, want exactly 1", asked)
	}

	if err := Forget("host-a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decide("host-a", BringAsk, true, yes); err != nil {
		t.Fatal(err)
	}
	if asked != 2 {
		t.Errorf("after forget, asked %d times, want 2", asked)
	}
}

// never and always must not consult the user at all, and a
// non-interactive session must never block on a question.
func TestDecideModes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	boom := func(string) (bool, error) { return false, errors.New("must not ask") }

	if bring, _, err := Decide("h", BringNever, true, boom); err != nil || bring {
		t.Errorf("never: bring=%v err=%v", bring, err)
	}
	if bring, _, err := Decide("h", BringAlways, true, boom); err != nil || !bring {
		t.Errorf("always: bring=%v err=%v", bring, err)
	}
	if bring, remembered, err := Decide("h2", BringAsk, false, boom); err != nil || bring || remembered {
		t.Errorf("non-interactive ask: bring=%v remembered=%v err=%v", bring, remembered, err)
	}
}

// A corrupt decisions file resets to "ask again" rather than to a
// remembered yes. It is preference state, not security state, but the
// safe direction is still the one that touches fewer hosts.
func TestCorruptDecisionsFileAsksAgain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	path := DecisionsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	asked := false
	if _, _, err := Decide("h", BringAsk, true, func(string) (bool, error) { asked = true; return false, nil }); err != nil {
		t.Fatal(err)
	}
	if !asked {
		t.Error("a corrupt decisions file was treated as an answer")
	}
}

func TestBinaryForRefusesUnsupportedPlatforms(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if _, err := BinaryFor(Probe{OS: "windows", Arch: "amd64"}); !errors.Is(err, errNoBinary) {
		t.Errorf("windows: err = %v, want errNoBinary", err)
	}

	// A platform we have no cached build for must name the exact command
	// that fixes it — gish does not download release artifacts (#112).
	_, err := BinaryFor(Probe{OS: "linux", Arch: "riscv64"})
	if !errors.Is(err, errNoBinary) {
		t.Fatalf("err = %v, want errNoBinary", err)
	}
	if !strings.Contains(err.Error(), "GOOS=linux GOARCH=riscv64") {
		t.Errorf("error does not tell the user how to fix it: %v", err)
	}
}

// Self-copy: when the far side matches this process, the binary to send
// is the one already running, with no cache involved.
func TestBinaryForUsesSelfOnMatchingPlatform(t *testing.T) {
	got, err := BinaryFor(Probe{OS: goos(), Arch: goarch()})
	if err != nil {
		t.Fatalf("self-copy: %v", err)
	}
	if got == "" {
		t.Error("self-copy returned no path")
	}
}

func TestREADMEExplainsItself(t *testing.T) {
	// Someone finding this file is a sysadmin deciding whether to open a
	// ticket. It has to answer their questions without them asking us.
	for _, want := range []string{"not a daemon", "did not modify any dotfile", "--uninstall", "github.com/blairham/gish"} {
		if !strings.Contains(readmeText, want) {
			t.Errorf("README does not say %q", want)
		}
	}
}

// The whole flow under the local transport: decide, probe, push,
// verify, drop the README, and produce the exec line — then do it again
// and confirm the repeat visit copies nothing.
func TestBringEndToEnd(t *testing.T) {
	h := newHarness(t)
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("HOME", data)
	t.Setenv("USERPROFILE", data)
	t.Setenv("GISH_THEME", "p10k")

	// Pose as a small binary rather than shipping the test executable.
	fake := filepath.Join(t.TempDir(), "gish")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // fake binary in a temp dir
		t.Fatal(err)
	}
	prev := selfPath
	selfPath = func() string { return fake }
	t.Cleanup(func() { selfPath = prev })

	opts := Options{
		Host:        "example",
		Mode:        BringAlways,
		Interactive: false,
		Stderr:      io.Discard,
	}
	sess, err := Bring(context.Background(), h.local, opts)
	if err != nil {
		t.Fatalf("bring: %v", err)
	}
	if !sess.Pushed {
		t.Error("first visit did not push")
	}
	if !strings.Contains(sess.Command, "--remote-session") {
		t.Errorf("command %q lacks --remote-session", sess.Command)
	}

	// Binary, config, and README all landed, with the config readable
	// only by its owner.
	for _, name := range []string{sess.Binary.Name, sess.Config.Name, "README"} {
		if _, err := os.Stat(filepath.Join(sess.Probe.Dir, name)); err != nil {
			t.Errorf("%s did not land: %v", name, err)
		}
	}
	info, err := os.Stat(filepath.Join(sess.Probe.Dir, sess.Config.Name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", info.Mode().Perm())
	}
	cfg, err := os.ReadFile(filepath.Join(sess.Probe.Dir, sess.Config.Name)) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "GISH_THEME='p10k'") {
		t.Errorf("theme did not travel:\n%s", cfg)
	}

	// Repeat visit: same content, so nothing is copied again.
	again, err := Bring(context.Background(), h.local, opts)
	if err != nil {
		t.Fatalf("second bring: %v", err)
	}
	if again.Pushed {
		t.Error("repeat visit re-pushed an identical binary")
	}
}

// GISH_SSH_BRING=never must reach no further than the decision: no
// probe, no connection, nothing touched on the remote.
func TestBringNeverTouchesTheRemote(t *testing.T) {
	h := newHarness(t)
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("HOME", data)
	t.Setenv("USERPROFILE", data)

	if _, err := Bring(context.Background(), h.local, Options{
		Host: "example", Mode: BringNever, Stderr: io.Discard,
	}); err == nil {
		t.Fatal("never mode returned a session")
	}
	if _, err := os.Stat(filepath.Join(h.home, ".cache", "gish")); err == nil {
		t.Error("never mode created a directory on the remote")
	}
}
