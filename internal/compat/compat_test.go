package compat_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// -update regenerates docs/compat.md from a live run; without it the
// test is the CI regression gate (#101): the corpus must not pass
// fewer cases than the published scoreboard records.
var update = flag.Bool("update", false, "regenerate docs/compat.md")

const scoreboardPath = "../../docs/compat.md"

// recordedRe pulls the pass count out of the published headline.
var recordedRe = regexp.MustCompile(`\*\*(\d+) of (\d+) cases pass`)

// recordedBashRe pulls the oracle version the scoreboard was generated
// against. The pass count is only comparable against the same bash
// major: macOS ships bash 3.2 (2007), where *bash itself* rejects
// `${s,,}` and `declare -A` and koi is ahead of the oracle.
var recordedBashRe = regexp.MustCompile(`against bash (\d+)\.`)

func TestCompatScoreboard(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this machine: the differential oracle is unavailable")
	}
	koiBin := buildKoi(t)

	results := compat.RunAll(context.Background(), bashBin, koiBin)
	summary := compat.Summarize(results)

	if *update {
		doc := compat.Scoreboard(results, bashVersion(t, bashBin))
		if err := os.WriteFile(scoreboardPath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s: %d/%d (%d%%)", scoreboardPath, summary.Passed, summary.Total, summary.Rate())
		return
	}

	// Regression gate: never fewer passes than published — but only
	// against the same bash major the scoreboard was generated with.
	recorded, recordedTotal := readRecorded(t)
	if got, want := bashMajor(t, bashBin), recordedBashMajor(t); got != want {
		t.Skipf("oracle is bash %s, scoreboard was generated against bash %s: "+
			"pass counts are not comparable across majors (this run: %d/%d)",
			got, want, summary.Passed, summary.Total)
	}
	if summary.Passed < recorded {
		for _, f := range compat.Failures(results) {
			t.Logf("FAIL %s (%s): %s — %s", f.Name, f.Category, f.Reason, f.Diff())
		}
		t.Fatalf("compat regression: %d/%d passing, scoreboard records %d/%d — run `make compat` after fixing",
			summary.Passed, summary.Total, recorded, recordedTotal)
	}
	if summary.Passed > recorded {
		t.Logf("compat improved: %d passing vs %d recorded — run `make compat` to publish",
			summary.Passed, recorded)
	}
}

// TestCorpusIsWellFormed guards the corpus itself: a scoreboard whose
// cases are duplicated or unattributed cannot be argued with.
func TestCorpusIsWellFormed(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, c := range compat.Corpus {
		if seen[c.Name] {
			t.Errorf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
		if c.Provenance == "" {
			t.Errorf("case %q has no provenance", c.Name)
		}
		if strings.TrimSpace(c.Script) == "" {
			t.Errorf("case %q has an empty script", c.Name)
		}
		if c.Category == "" {
			t.Errorf("case %q has no category", c.Name)
		}
	}
}

// buildKoi is the binary every differential suite measures. By default it
// builds the working tree, which answers "is this branch correct?".
//
// $KOI_BIN answers the other question, which had no way to be asked (#284):
// "is the koi on this machine correct?". Those two diverge silently — an
// installed koi was found 15 commits behind a green main, failing 15 of the
// 17 agent-gate cases, among them `exec -a` (#241) on Claude Code's find and
// grep shims and `set -Eeuo pipefail` (#245) on every strict-mode script.
// Nothing reported it, because nothing was testing that binary.
//
//	KOI_BIN=$(command -v koi-bash) go test ./internal/compat/ -run TestAgentGate
//
// An unusable $KOI_BIN is fatal rather than a fall back to building. Falling
// back would let a mistyped path report a green run of a binary nobody asked
// for, which is the failure this exists to catch, wearing a pass.
func buildKoi(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("KOI_BIN"); bin != "" {
		abs, err := resolveKoiBin(bin)
		if err != nil {
			t.Fatal(err)
		}
		// Name it, so a green run is attributable to a known binary rather
		// than to whatever happened to be on disk at the time.
		t.Logf("gating installed binary %s (%s)", abs, koiVersion(t, abs))
		return abs
	}
	bin := filepath.Join(t.TempDir(), "koi")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/koi")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build koi: %v\n%s", err, out)
	}
	return bin
}

// resolveKoiBin validates a $KOI_BIN value and returns it absolute. Split
// out of buildKoi so the rejections are testable: exercising them through
// buildKoi means tripping t.Fatal, and a subtest that fatals is a failed
// subtest whatever its parent concludes about it.
func resolveKoiBin(bin string) (string, error) {
	abs, err := filepath.Abs(bin)
	if err != nil {
		return "", fmt.Errorf("KOI_BIN=%q: %w", bin, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("KOI_BIN=%q: %w", bin, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("KOI_BIN=%q: not an executable file", bin)
	}
	return abs, nil
}

// koiVersion is the binary's self-reported version, or the reason it could
// not be read. Only ever used to label a run, so it never fails one.
func koiVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "version unreadable: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}

// TestKoiBinOverride covers the #284 escape hatch itself. Without this the
// override could stop working and every suite would quietly go back to
// gating a fresh build while the Makefile target still claimed otherwise —
// the same class of silent pass the override exists to end.
func TestKoiBinOverride(t *testing.T) {
	// A path that is not the working tree's build, so a fall back to
	// building would be visible rather than coincidentally identical.
	dir := t.TempDir()
	bin := filepath.Join(dir, "koi-under-test")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/koi").CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}

	t.Run("honors an executable path", func(t *testing.T) {
		t.Setenv("KOI_BIN", bin)
		if got := buildKoi(t); got != bin {
			t.Errorf("buildKoi() = %q, want the KOI_BIN path %q", got, bin)
		}
	})

	t.Run("resolves a relative path", func(t *testing.T) {
		t.Setenv("KOI_BIN", filepath.Join(dir, ".", "koi-under-test"))
		if got := buildKoi(t); got != bin {
			t.Errorf("buildKoi() = %q, want the absolute path %q", got, bin)
		}
	})

	t.Run("unset still builds the working tree", func(t *testing.T) {
		t.Setenv("KOI_BIN", "")
		got := buildKoi(t)
		if got == bin {
			t.Fatalf("buildKoi() returned the fixture with KOI_BIN unset")
		}
		if _, err := os.Stat(got); err != nil {
			t.Errorf("buildKoi() = %q, which does not exist: %v", got, err)
		}
	})

	// The rejections are the point: each one is a way to run green against a
	// binary the caller did not mean, so each must stop the run instead.
	for _, tc := range []struct{ name, path string }{
		{"missing file", filepath.Join(dir, "no-such-koi")},
		{"a directory", dir},
		{"not executable", writeFile(t, filepath.Join(dir, "plain"), 0o644)},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if _, err := resolveKoiBin(tc.path); err == nil {
				t.Errorf("resolveKoiBin(%q) = nil error; want a rejection", tc.path)
			}
		})
	}

	// A build is not silently substituted for a bad path: buildKoi must be
	// reached only after the resolver agrees.
	t.Run("accepts the fixture", func(t *testing.T) {
		got, err := resolveKoiBin(bin)
		if err != nil {
			t.Fatalf("resolveKoiBin(%q) = %v; want it accepted", bin, err)
		}
		if got != bin {
			t.Errorf("resolveKoiBin(%q) = %q, want %q", bin, got, bin)
		}
	})
}

// writeFile creates a file with the given mode and returns its path.
func writeFile(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("not a binary\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// bashMajor is the oracle's major version ("5"), or "?" when unknown.
func bashMajor(t *testing.T, bashBin string) string {
	t.Helper()
	v := bashVersion(t, bashBin)
	if i := strings.Index(v, "bash "); i >= 0 {
		v = v[i+len("bash "):]
	}
	if i := strings.IndexByte(v, '.'); i > 0 {
		return v[:i]
	}
	return "?"
}

// recordedBashMajor is the major version the scoreboard records.
func recordedBashMajor(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(scoreboardPath)
	if err != nil {
		return "?"
	}
	if m := recordedBashRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return "?"
}

func bashVersion(t *testing.T, bashBin string) string {
	t.Helper()
	out, err := exec.Command(bashBin, "-c", "echo $BASH_VERSION").Output()
	if err != nil {
		return "bash (version unknown)"
	}
	return "bash " + strings.TrimSpace(string(out))
}

func readRecorded(t *testing.T) (passed, total int) {
	t.Helper()
	data, err := os.ReadFile(scoreboardPath)
	if err != nil {
		t.Fatalf("%s missing — run `make compat` to create it: %v", scoreboardPath, err)
	}
	m := recordedRe.FindSubmatch(data)
	if m == nil {
		t.Fatalf("%s has no headline count", scoreboardPath)
	}
	passed, _ = strconv.Atoi(string(m[1])) //nolint:errcheck // regex guarantees digits
	total, _ = strconv.Atoi(string(m[2]))  //nolint:errcheck // as above
	return passed, total
}

// TestScoreboardMatchesCorpusSize catches the doc drifting from the
// corpus without a regeneration.
func TestScoreboardMatchesCorpusSize(t *testing.T) {
	data, err := os.ReadFile(scoreboardPath)
	if err != nil {
		t.Skipf("no scoreboard yet: %v", err)
	}
	m := recordedRe.FindSubmatch(data)
	if m == nil {
		t.Fatal("scoreboard has no headline count")
	}
	total, _ := strconv.Atoi(string(m[2])) //nolint:errcheck // regex guarantees digits
	if total != len(compat.Corpus) {
		t.Errorf("scoreboard covers %d cases, corpus has %d — run `make compat`",
			total, len(compat.Corpus))
	}
	fmt.Fprintln(os.Stderr) // keep -v output readable
}

// The corpus runs with a scratch $HOME, not the developer's (#260).
//
// This is not tidiness. `make compat` writes the published scoreboard, so
// a case that reads anything under $HOME would be scored against whatever
// is in one person's home directory and the number would not be
// reproducible between machines — and a case that *wrote* would write into
// a real home. Nothing in the corpus does either today, which is exactly
// the state in which a rule like this quietly stops being true.
func TestRunnerDoesNotUseTheRealHome(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed: nothing to be differential against")
	}
	koiBin := buildKoi(t)

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to protect")
	}

	// Read back what each shell was actually given, through the same
	// entry point the scoreboard uses — testing runScript directly would
	// not prove the corpus path is sandboxed.
	r := compat.Run(context.Background(), bashBin, koiBin, compat.Case{
		Name:   "home probe",
		Script: `echo "$HOME"; echo "${XDG_CONFIG_HOME:-unset}"`,
	})
	for _, got := range []struct{ shell, out string }{
		{"bash", r.BashOut}, {"koi", r.KoiOut},
	} {
		home, xdg, _ := strings.Cut(strings.TrimSpace(got.out), "\n")
		if home == realHome {
			t.Errorf("%s ran with the real $HOME (%s)", got.shell, home)
		}
		if home == "" {
			t.Errorf("%s ran with no $HOME at all", got.shell)
		}
		// The XDG roots have to move too: koi resolves its rc through
		// them before $HOME, so a scratch home alone would still let a
		// real ~/.config/koi decide the score.
		if !strings.HasPrefix(xdg, home) {
			t.Errorf("%s: XDG_CONFIG_HOME = %q, which is not inside its $HOME %q", got.shell, xdg, home)
		}
	}

	// And a case that writes lands in the scratch dir, which is gone
	// afterwards — the half a read-only probe cannot show.
	//
	// The witness name is unique per run rather than fixed. A fixed one
	// is self-poisoning: the first failing run leaves the file in the
	// real home, and every later run then fails on that leftover instead
	// of on what it did itself. (Which is how this comment came to be
	// written.)
	witness := "koi-compat-witness-" + filepath.Base(t.TempDir())
	w := compat.Run(context.Background(), bashBin, koiBin, compat.Case{
		Name:   "home write probe",
		Script: `echo marker > "$HOME/` + witness + `"; cat "$HOME/` + witness + `"`,
	})
	if !w.Pass {
		t.Errorf("writing under the scratch home did not work the same in both shells: %s\n  bash: %q\n  koi: %q",
			w.Reason, w.BashOut, w.KoiOut)
	}
	if path := filepath.Join(realHome, witness); fileExists(path) {
		os.Remove(path) //nolint:errcheck // best effort; the failure below is the point
		t.Fatalf("a corpus case wrote %s into the real home directory", witness)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
