package promptengine

import (
	"runtime"
	"strings"
	"testing"
)

// The system-state segments (#132). What is testable portably is the
// shape of the answers and the quiet-by-default rules; the platform
// readers are exercised on the platform that has them.

func TestHumanBytesReadsAtAGlance(t *testing.T) {
	t.Parallel()

	tests := map[uint64]string{
		0:                      "0B",
		512:                    "512B",
		1024:                   "1K",
		1536:                   "1.5K",
		10 * 1024:              "10K",
		1024 * 1024:            "1M",
		3 * 1024 * 1024 * 1024: "3G",
	}
	for n, want := range tests {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// disk_usage stays quiet below the warning level: a segment that is
// always there is a decoration, and this one exists to be a warning.
func TestDiskUsageIsQuietUntilItMatters(t *testing.T) {
	t.Parallel()

	ctx := sampleContext()
	ctx.Cwd = t.TempDir() // a real filesystem, whatever this machine has
	cfg := Preset("lean")

	if _, ok := renderDiskUsage(cfg, ctx); ok {
		// Only fails on a machine whose temp filesystem is genuinely
		// over 90% full, in which case the segment is right to speak.
		used, total, statOK := diskUsage(ctx.Cwd)
		if statOK && float64(used)/float64(total) < 0.9 {
			t.Error("disk_usage rendered on a filesystem with room")
		}
	}

	cfg.Set("DISK_USAGE_WARNING_LEVEL", "0")
	if _, ok := renderDiskUsage(cfg, ctx); !ok && runtime.GOOS != "windows" {
		t.Error("disk_usage stayed quiet with the warning level at zero")
	}
}

// Load is read per core, because 4.0 is idle on a sixteen-core machine
// and desperate on a laptop.
func TestLoadStateIsPerCore(t *testing.T) {
	if _, ok := systemLoad(); !ok {
		t.Skip("no load average on this platform")
	}
	rendered, ok := renderLoad(Preset("lean"), sampleContext())
	if !ok {
		t.Fatal("load rendered nothing on a platform that has it")
	}
	if !strings.Contains(rendered.Content, ".") {
		t.Errorf("load = %q, want a decimal", rendered.Content)
	}
	switch rendered.State {
	case "NORMAL", "WARNING", "CRITICAL":
	default:
		t.Errorf("state = %q, want one of the three levels", rendered.State)
	}
}

// An unavailable metric renders nothing rather than a zero — a prompt
// that says "0B" is worse than one that says nothing.
func TestUnavailableMetricsRenderNothing(t *testing.T) {
	t.Parallel()

	cfg := Preset("lean")
	if _, _, ok := systemMemory(); !ok {
		if _, rendered := renderRAM(cfg, sampleContext()); rendered {
			t.Error("ram rendered without a memory reading")
		}
	}
	if _, _, ok := batteryStatus(); !ok {
		if _, rendered := renderBattery(cfg, sampleContext()); rendered {
			t.Error("battery rendered without a battery")
		}
	}
}
