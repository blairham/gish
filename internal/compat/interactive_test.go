//go:build unix

package compat_test

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"

	"github.com/blairham/gish/internal/compat"
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

func TestInteractiveGates(t *testing.T) {
	bashBin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this machine: the differential oracle is unavailable")
	}
	gishBin := buildGish(t)
	ctx := context.Background()

	pasteResults := compat.RunPasteAll(ctx, bashBin, gishBin)
	sourceResults := compat.RunSourceAll(ctx, bashBin, gishBin, t.TempDir())

	pastePassed := 0
	for _, r := range pasteResults {
		if !r.Pass {
			t.Logf("PASTE FAIL %s: %s\n  bash=%q (%d)\n  gish=%q (%d)",
				r.Name, r.Reason, r.BashOut, r.BashCode, r.GishOut, r.GishCode)
			continue
		}
		pastePassed++
	}
	sourceRan, sourcePassed := 0, 0
	for _, r := range sourceResults {
		if !r.Present {
			t.Logf("SOURCE SKIP %s: not installed on this machine", r.Name)
			continue
		}
		sourceRan++
		if !r.Pass {
			t.Logf("SOURCE FAIL %s: %s\n  bash=%q (%d)\n  gish=%q (%d)",
				r.Name, r.Reason, r.BashOut, r.BashCode, r.GishOut, r.GishCode)
			continue
		}
		sourcePassed++
	}

	if *update {
		doc := compat.InteractiveDoc(pasteResults, sourceResults, bashVersion(t, bashBin))
		if err := os.WriteFile(interactivePath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s: paste %d/%d, source %d/%d present",
			interactivePath, pastePassed, len(pasteResults), sourcePassed, sourceRan)
		return
	}

	// The paste gate is hermetic — it depends on nothing but bash and
	// gish — so it is enforced as an absolute count.
	recordedPaste, recordedPasteTotal := readInteractiveRecorded(t, pasteRecordedRe)
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
