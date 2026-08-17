//go:build linux

package promptengine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// System state on Linux: /proc and /sys reads, plus one statfs (#132).
//
// The rule this package holds — a segment may read memory, environment
// and files, but may not fork, dial or block — is where the 5.9ms in
// docs/bench.md comes from. These segments were listed as impossible
// under it, and on Linux that was simply wrong: memory, load, swap and
// battery are all *files*, and disk usage is one syscall. Nothing here
// runs a process.

// systemMemory reports used and total bytes.
//
// "Used" is total minus MemAvailable rather than minus MemFree, because
// MemFree counts the page cache as unavailable and produces the "my
// machine has no memory left" number that people learn to ignore.
func systemMemory() (used, total uint64, ok bool) {
	fields := readMeminfo()
	total, hasTotal := fields["MemTotal"]
	available, hasAvail := fields["MemAvailable"]
	if !hasTotal || !hasAvail || total == 0 {
		return 0, 0, false
	}
	return total - available, total, true
}

// systemSwap reports used and total swap bytes.
func systemSwap() (used, total uint64, ok bool) {
	fields := readMeminfo()
	total, hasTotal := fields["SwapTotal"]
	free, hasFree := fields["SwapFree"]
	if !hasTotal || !hasFree || total == 0 {
		return 0, 0, false
	}
	return total - free, total, true
}

// readMeminfo parses /proc/meminfo into bytes.
func readMeminfo() map[string]uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	out := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			value *= 1024
		}
		out[name] = value
	}
	return out
}

// systemLoad reports the one-minute load average.
func systemLoad() (float64, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return load, true
}

// diskUsage reports used and total bytes for the filesystem holding path.
func diskUsage(path string) (used, total uint64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	//nolint:gosec // block counts and sizes are non-negative by construction
	bsize := uint64(st.Bsize)
	total = st.Blocks * bsize
	// Available rather than free: the reserved blocks are not yours, and
	// counting them is how a "20% free" prompt is wrong at exactly the
	// moment it matters.
	free := st.Bavail * bsize
	if total == 0 {
		return 0, 0, false
	}
	return total - free, total, true
}

// batteryStatus reports charge percentage and whether it is charging.
func batteryStatus() (percent int, charging, ok bool) {
	matches, err := filepath.Glob("/sys/class/power_supply/BAT*")
	if err != nil || len(matches) == 0 {
		return 0, false, false
	}
	dir := matches[0]
	capacity := strings.TrimSpace(readFile(filepath.Join(dir, "capacity")))
	if capacity == "" {
		return 0, false, false
	}
	percent, err = strconv.Atoi(capacity)
	if err != nil {
		return 0, false, false
	}
	status := strings.TrimSpace(readFile(filepath.Join(dir, "status")))
	return percent, status == "Charging" || status == "Full", true
}
