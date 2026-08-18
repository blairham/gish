//go:build !darwin && !linux

package interp

// No resource limits to report. An empty table is what makes `ulimit`
// keep answering "unsupported builtin" on platforms without rlimits,
// which is the honest refusal it already gave everywhere — better than
// inventing a number a script would then act on.
var ulimitSpecs []ulimitSpec

const rlimInfinity = ^uint64(0)

func isUnlimited(raw uint64) bool { return raw == rlimInfinity }

func getRlimit(int, bool) (uint64, error) { return 0, errNoRlimits }

func setRlimit(int, bool, bool, uint64) error { return errNoRlimits }
