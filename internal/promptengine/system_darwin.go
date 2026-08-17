//go:build darwin

package promptengine

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// System state on macOS: sysctl and one statfs (#132).
//
// Less is available here than on Linux, and the honest thing is to say
// so per metric rather than to reach for a subprocess. `vm_stat` and
// `pmset` would answer the remaining two, and a prompt that forks twice
// is the thing this engine exists not to be:
//
//   - load and swap come from sysctl, which is a syscall;
//   - disk usage is statfs;
//   - **memory** needs mach's host_statistics64, which is not reachable
//     through sysctl and needs cgo. hw.memsize gives the total, and a
//     total with no used figure is not a segment worth rendering;
//   - **battery** needs IOKit, same reason.
//
// The two that are missing render nothing, which is what they did
// before — the difference is that `prompt show` now says why.

// systemMemory is unavailable on macOS without cgo: see above.
func systemMemory() (used, total uint64, ok bool) { return 0, 0, false }

// systemSwap reads vm.swapusage, which sysctl returns as an xsw_usage
// struct: three 64-bit byte counts (total, avail, used) followed by an
// encumbered flag.
func systemSwap() (used, total uint64, ok bool) {
	raw, err := unix.SysctlRaw("vm.swapusage")
	if err != nil || len(raw) < 24 {
		return 0, 0, false
	}
	total = binary.NativeEndian.Uint64(raw[0:8])
	used = binary.NativeEndian.Uint64(raw[16:24])
	if total == 0 {
		return 0, 0, false
	}
	return used, total, true
}

// systemLoad reads vm.loadavg, a struct loadavg: three fixed-point
// values and the scale they are expressed in.
func systemLoad() (float64, bool) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 16 {
		return 0, false
	}
	scale := binary.NativeEndian.Uint32(raw[12:16])
	if scale == 0 {
		return 0, false
	}
	one := binary.NativeEndian.Uint32(raw[0:4])
	return float64(one) / float64(scale), true
}

// diskUsage reports used and total bytes for the filesystem holding path.
func diskUsage(path string) (used, total uint64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bsize := uint64(st.Bsize)
	total = st.Blocks * bsize
	// Available, not free: the reserved blocks are not yours.
	free := st.Bavail * bsize
	if total == 0 {
		return 0, 0, false
	}
	return total - free, total, true
}

// batteryStatus is unavailable on macOS without IOKit: see above.
func batteryStatus() (percent int, charging, ok bool) { return 0, false, false }
