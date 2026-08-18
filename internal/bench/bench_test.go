//go:build unix

package bench_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/blairham/koi-shell/internal/bench"
)

// -update regenerates docs/bench.md from a live run. Without it the
// package's tests only check that the harness works, because timing
// numbers must never be a CI gate — a loaded runner would fail an
// honest shell (#37's budget gate is the koi-vs-koi guard).
var update = flag.Bool("update", false, "regenerate docs/bench.md")

const (
	reportPath     = "../../docs/bench.md"
	startupRuns    = 7
	keystrokeCount = 40
)

func TestBenchmarkReport(t *testing.T) {
	if !*update {
		t.Skip("timing runs are opt-in: `make bench` regenerates docs/bench.md")
	}
	koiBin := buildKoi(t)

	configs := bench.StartupConfigs(koiBin)
	// The head-to-head for the native p10k port. Reported as missing
	// rather than omitted when upstream is not installed, so a reader
	// can tell "we lost that row" from "we never ran it".
	if p10k, ok := bench.PowerlevelConfig(); ok {
		configs = append(configs, p10k)
	} else {
		configs = append(configs, bench.Config{
			Label: "zsh + powerlevel10k",
			Note:  "powerlevel10k not installed on the measuring machine",
		})
	}
	if real, ok := bench.RealZshConfig(); ok {
		configs = append(configs, real)
	}
	startups := make([]bench.StartupResult, 0, len(configs))
	for _, cfg := range configs {
		r := bench.MeasureStartup(cfg, startupRuns)
		if r.Available && r.Err != "" {
			t.Logf("%s: %s", r.Label, r.Err)
		}
		startups = append(startups, r)
	}

	keystrokes := bench.MeasureKeystrokes(koiBin, bench.KeystrokeScenarios(), keystrokeCount)
	for _, k := range keystrokes {
		if k.Err != "" {
			t.Logf("%s: %s", k.Scenario, k.Err)
		}
	}

	report := bench.Report(startups, keystrokes, startupRuns, keystrokeCount)
	if err := os.WriteFile(reportPath, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, r := range startups {
		if r.Available && r.Err == "" {
			t.Logf("%-40s median %6.1fms best %6.1fms", r.Label,
				float64(r.Median().Microseconds())/1000, float64(r.Best().Microseconds())/1000)
		}
	}
	for _, k := range keystrokes {
		if k.Err == "" {
			t.Logf("%-40s p50 %6.2fms p99 %6.2fms", k.Scenario,
				float64(k.P50().Microseconds())/1000, float64(k.P99().Microseconds())/1000)
		}
	}
}

// TestHarnessMeasuresSomething keeps the harness itself honest in
// normal CI: one koi launch must be measurable, and a missing shell
// must be reported rather than silently dropped.
func TestHarnessMeasuresSomething(t *testing.T) {
	koiBin := buildKoi(t)
	configs := bench.StartupConfigs(koiBin)
	if len(configs) == 0 {
		t.Fatal("no configs built")
	}

	r := bench.MeasureStartup(configs[0], 1)
	if !r.Available || r.Err != "" {
		t.Fatalf("koi startup unmeasurable: %+v", r)
	}
	if r.Median() <= 0 {
		t.Errorf("median = %v", r.Median())
	}

	missing := bench.MeasureStartup(bench.Config{Label: "ghost shell"}, 1)
	if missing.Available || missing.Err != "not installed" {
		t.Errorf("missing shell should be reported: %+v", missing)
	}
}

func buildKoi(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "koi")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/koi")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build koi: %v\n%s", err, out)
	}
	return bin
}
