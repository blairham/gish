//go:build !linux && !darwin

package promptengine

// System state is unimplemented on this platform (#132). The segments
// render nothing, which is what an absent metric should look like — and
// `prompt show` lists them so the absence is visible.

func systemMemory() (used, total uint64, ok bool)     { return 0, 0, false }
func systemSwap() (used, total uint64, ok bool)       { return 0, 0, false }
func systemLoad() (float64, bool)                     { return 0, false }
func diskUsage(string) (used, total uint64, ok bool)  { return 0, 0, false }
func batteryStatus() (percent int, charging, ok bool) { return 0, false, false }
