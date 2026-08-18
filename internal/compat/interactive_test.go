//go:build unix

package compat_test

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"

	"github.com/blairham/koi-shell/internal/compat"
)

// The interactive gates (#161): pasted lines and sourced init scripts,
// published to docs/interactive-compat.md and enforced from it.
//
// Same discipline as the script scoreboard — regenerate with -update,
// and without it the test refuses a lower pass count than the doc
// records. The gates caught a real bug on their first run: a multi-line
// paste lost its newlines, so a pasted heredoc arrived as
// `cat <<'EOF'line oneline two`.

const interactivePath = "../../docs/interactive-compat.md"

// Only the paste gate's headline is read back: it is hermetic, so an
// absolute count means something. The source gate's numbers describe
// whichever tools the generating machine had installed, and enforcing
// those would fail an honest run on a machine with fewer of them.
var pasteRecordedRe = regexp.MustCompile(`\*\*Paste gate: (\d+) of (\d+) cases pass`)

// gatesEnv opts the pty gates in. `make paste-gate` and
// `make paste-gate-check` set it; a plain `go test ./...` does not.
//
// They are skipped by default because they are the most expensive tests
// in the repository — eighteen real shells on real ptys, plus every
// installed tool's init — and running them *concurrently with the rest
// of the suite* is what made them flaky rather than slow: on a
// two-core runner under -race, a shell missed its own first prompt
// inside a minute. The dedicated CI step is the enforcement point, and
// running them twice per push bought nothing but the flake.
const gatesEnv = "KOI_GATES"

func TestInteractiveGates(t *testing.T) {
	if os.Getenv(gatesEnv) == "" {
		t.Skipf("set %s=1 to run the pty gates (make paste-gate-check)", gatesEnv)
	}
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this machine: the differential oracle is unavailable")
	}
	koiBin := buildKoi(t)
	ctx := context.Background()

	pasteResults := compat.RunPasteAll(ctx, bashBin, koiBin)
	sourceResults := compat.RunSourceAll(ctx, bashBin, koiBin, t.TempDir())
	ecoResults := compat.RunEcosystemAll(ctx, koiBin)

	pastePassed, pasteSkipped := 0, 0
	for _, r := range pasteResults {
		switch {
		case r.Skipped:
			// The oracle on this machine cannot answer — macOS still
			// ships bash 3.2.57 — so the case is neither passed nor
			// failed, and the recorded count is adjusted rather than the
			// gate disabled.
			pasteSkipped++
			t.Logf("PASTE SKIP %s: %s", r.Name, r.Reason)
		case !r.Pass:
			t.Logf("PASTE FAIL %s: %s\n  bash=%q (%d)\n  koi=%q (%d)",
				r.Name, r.Reason, r.BashOut, r.BashCode, r.KoiOut, r.KoiCode)
		default:
			pastePassed++
		}
	}
	sourceRan, sourcePassed := 0, 0
	for _, r := range sourceResults {
		switch {
		case !r.Present:
			t.Logf("SOURCE SKIP %s: not installed on this machine", r.Name)
		case r.Unstable:
			// The oracle misbehaved (a helper program dying of SIGPIPE
			// as the script exits). Not koi's failure, and not a pass
			// either: it is a case that could not be judged.
			t.Logf("SOURCE UNJUDGED %s: %s\n  bash=%q (%d)", r.Name, r.Reason, r.BashOut, r.BashCode)
		case !r.Pass:
			sourceRan++
			t.Logf("SOURCE FAIL %s: %s\n  bash=%q (%d)\n  koi=%q (%d)",
				r.Name, r.Reason, r.BashOut, r.BashCode, r.KoiOut, r.KoiCode)
		default:
			sourceRan++
			sourcePassed++
		}
	}

	ecoRan, ecoPassed := 0, 0
	for _, r := range ecoResults {
		if !r.Present {
			t.Logf("ECOSYSTEM SKIP %s: not installed on this machine", r.Name)
			continue
		}
		ecoRan++
		if !r.Pass {
			t.Logf("ECOSYSTEM FAIL %s: %s\n  session output: %q", r.Name, r.Reason, r.Output)
			continue
		}
		ecoPassed++
	}

	if *update {
		doc := compat.InteractiveDoc(pasteResults, sourceResults, ecoResults, bashVersion(t, bashBin))
		if err := os.WriteFile(interactivePath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s: paste %d/%d, source %d/%d present, ecosystem %d/%d present",
			interactivePath, pastePassed, len(pasteResults), sourcePassed, sourceRan, ecoPassed, ecoRan)
		return
	}

	// The paste gate is hermetic — it depends on nothing but bash and
	// koi — so it is enforced as an absolute count.
	recordedPaste, recordedPasteTotal := readInteractiveRecorded(t, pasteRecordedRe)
	// Skipped cases are subtracted from what the doc recorded, so a
	// machine with an older oracle still enforces every case it *could*
	// run. Anything else would either fail an honest run or, worse,
	// quietly stop gating.
	recordedPaste -= pasteSkipped
	if pastePassed < recordedPaste {
		t.Errorf("paste gate regression: %d/%d passing, the doc records %d/%d — run `make paste-gate` after fixing",
			pastePassed, len(pasteResults), recordedPaste, recordedPasteTotal)
	}

	// The source gate depends on what is installed here, which differs
	// between a CI runner and a laptop. Enforcing an absolute count
	// would fail honestly-passing runs on machines with fewer tools, so
	// the rule is "everything present must pass".
	if sourcePassed < sourceRan {
		t.Errorf("source gate: %d of %d installed tools failed", sourceRan-sourcePassed, sourceRan)
	}
	// Same rule for the ecosystem matrix, and for the same reason: what
	// a runner has installed is not a property of the shell.
	if ecoPassed < ecoRan {
		t.Errorf("ecosystem matrix: %d of %d installed tools failed", ecoRan-ecoPassed, ecoRan)
	}
	if pastePassed > recordedPaste {
		t.Logf("paste gate improved: %d vs %d recorded — run `make paste-gate` to publish",
			pastePassed, recordedPaste)
	}
}

func readInteractiveRecorded(t *testing.T, re *regexp.Regexp) (passed, total int) {
	t.Helper()
	data, err := os.ReadFile(interactivePath)
	if err != nil {
		t.Fatalf("read %s: %v (run `make paste-gate` to create it)", interactivePath, err)
	}
	m := re.FindStringSubmatch(string(data))
	if m == nil {
		t.Fatalf("%s has no recorded headline for %v", interactivePath, re)
	}
	passed, _ = strconv.Atoi(m[1])
	total, _ = strconv.Atoi(m[2])
	return passed, total
}

// The corpora must stay argue-with-able: every case named and sourced.
func TestInteractiveCorporaAreWellFormed(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, c := range compat.PasteCorpus {
		if c.Name == "" || c.Provenance == "" || c.Text == "" {
			t.Errorf("paste case %+v is missing a name, provenance or text", c)
		}
		if seen[c.Name] {
			t.Errorf("duplicate paste case %q", c.Name)
		}
		seen[c.Name] = true
	}
	for _, c := range compat.SourceCorpus {
		if c.Name == "" || c.Provenance == "" || c.Probe == "" || c.Locate == nil {
			t.Errorf("source case %+v is incomplete", c.Name)
		}
		if seen[c.Name] {
			t.Errorf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
	}
}
