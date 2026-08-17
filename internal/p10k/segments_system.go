package p10k

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// System-state segments (#132): ram, swap, load, disk_usage, battery.
//
// These were listed as impossible under this package's one rule — a
// segment may read memory, environment and files, but may not fork,
// dial or block — and on Linux that was simply wrong. Memory, swap,
// load and battery are files; disk usage is a syscall. What is actually
// impossible without a subprocess or cgo is per-platform and now says
// so per metric rather than per segment.
//
// The network group (public_ip, vpn_ip, nordvpn) and the trackers
// (taskwarrior, timewarrior) stay out. They are not a syscall away, and
// the mechanism they need — something else computes, the prompt renders
// what is known and marks it stale — is the one #130 built for git
// counts. Wiring them to it is a real design decision about how much a
// prompt should be quietly doing in the background, and it is worth
// making deliberately rather than because the mechanism happened to
// exist.

func init() {
	register("ram", renderRAM)
	register("swap", renderSwap)
	register("load", renderLoad)
	register("disk_usage", renderDiskUsage)
	register("battery", renderBattery)
}

func renderRAM(cfg *Config, _ *Context) (Rendered, bool) {
	used, total, ok := systemMemory()
	if !ok {
		return Rendered{}, false
	}
	return Rendered{
		Content: humanBytes(used) + "/" + humanBytes(total),
		Icon:    decodeEscapes(cfg.Icon("ram", "", "")),
	}, true
}

func renderSwap(cfg *Config, _ *Context) (Rendered, bool) {
	used, _, ok := systemSwap()
	if !ok || used == 0 {
		// Swap at zero is the normal state on a healthy machine, and a
		// segment that is always there saying "0B" is noise.
		return Rendered{}, false
	}
	return Rendered{
		Content: humanBytes(used),
		Icon:    decodeEscapes(cfg.Icon("swap", "", "")),
	}, true
}

// renderLoad colors by load per core, which is the only reading that
// means anything: 4.0 is idle on a 16-core machine and desperate on a
// laptop.
func renderLoad(cfg *Config, ctx *Context) (Rendered, bool) {
	load, ok := systemLoad()
	if !ok {
		return Rendered{}, false
	}
	cores := max(runtime.NumCPU(), 1)
	state := "NORMAL"
	switch ratio := load / float64(cores); {
	case ratio >= 1:
		state = "CRITICAL"
	case ratio >= 0.7:
		state = "WARNING"
	}
	return Rendered{
		Content: strconv.FormatFloat(load, 'f', 2, 64),
		Icon:    decodeEscapes(cfg.Icon("load", state, "")),
		State:   state,
	}, true
}

// renderDiskUsage shows the percentage in use, and only once it is
// worth knowing: upstream's default is to stay quiet below 90%, which
// is the difference between a warning and a decoration.
func renderDiskUsage(cfg *Config, ctx *Context) (Rendered, bool) {
	dir := ctx.Cwd
	if dir == "" {
		return Rendered{}, false
	}
	used, total, ok := diskUsage(dir)
	if !ok || total == 0 {
		return Rendered{}, false
	}
	pct := int(float64(used) / float64(total) * 100)

	state := "NORMAL"
	switch {
	case pct >= cfg.Int("DISK_USAGE_CRITICAL_LEVEL", 95):
		state = "CRITICAL"
	case pct >= cfg.Int("DISK_USAGE_WARNING_LEVEL", 90):
		state = "WARNING"
	}
	if state == "NORMAL" && !cfg.Bool("DISK_USAGE_ONLY_WARNING", false) {
		if pct < cfg.Int("DISK_USAGE_WARNING_LEVEL", 90) {
			return Rendered{}, false
		}
	}
	return Rendered{
		Content: strconv.Itoa(pct) + "%",
		Icon:    decodeEscapes(cfg.Icon("disk_usage", state, "")),
		State:   state,
	}, true
}

// renderBattery stays quiet on a charged machine that is plugged in —
// the state worth showing is the one that ends your work.
func renderBattery(cfg *Config, _ *Context) (Rendered, bool) {
	percent, charging, ok := batteryStatus()
	if !ok {
		return Rendered{}, false
	}
	state := "LOW"
	switch {
	case charging:
		state = "CHARGING"
	case percent >= cfg.Int("BATTERY_LOW_THRESHOLD", 20):
		state = "DISCONNECTED"
	}
	if state == "CHARGING" && percent >= 100 && !cfg.Bool("BATTERY_VERBOSE", false) {
		return Rendered{}, false
	}
	return Rendered{
		Content: strconv.Itoa(percent) + "%",
		Icon:    decodeEscapes(cfg.Icon("battery", state, "")),
		State:   state,
	}, true
}

// humanBytes renders a byte count the way a prompt should: two
// significant figures at most, because a prompt is read at a glance.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + "B"
	}
	value, exp := float64(n), 0
	for value >= unit && exp < 4 {
		value /= unit
		exp++
	}
	suffix := [...]string{"B", "K", "M", "G", "T"}[exp]
	if value >= 10 {
		return strconv.FormatFloat(value, 'f', 0, 64) + suffix
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0") + suffix
}
