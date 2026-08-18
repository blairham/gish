package interp

import "golang.org/x/sys/unix"

// The rows linux's bash reports, in its order.
//
// The order is the option letter's byte order, which is why -R comes
// first: uppercase sorts ahead of lowercase, and linux is the platform
// with an uppercase resource letter. Six of these have no darwin
// counterpart.
var ulimitSpecs = []ulimitSpec{
	{letter: 'R', label: "real-time non-blocking time", unit: "microseconds", factor: 1, resource: unix.RLIMIT_RTTIME},
	// "blocks" is bash's word and it means 1024 bytes here, not the 512
	// the name suggests — measured, because a round-trip through one
	// shell cannot tell the two apart and only a second reader of the
	// same kernel value can.
	{letter: 'c', label: "core file size", unit: "blocks", factor: 1024, resource: unix.RLIMIT_CORE},
	{letter: 'd', label: "data seg size", unit: "kbytes", factor: 1024, resource: unix.RLIMIT_DATA},
	{letter: 'e', label: "scheduling priority", factor: 1, resource: unix.RLIMIT_NICE},
	{letter: 'f', label: "file size", unit: "blocks", factor: 1024, resource: unix.RLIMIT_FSIZE},
	{letter: 'i', label: "pending signals", factor: 1, resource: unix.RLIMIT_SIGPENDING},
	{letter: 'l', label: "max locked memory", unit: "kbytes", factor: 1024, resource: unix.RLIMIT_MEMLOCK},
	{letter: 'm', label: "max memory size", unit: "kbytes", factor: 1024, resource: unix.RLIMIT_RSS},
	{letter: 'n', label: "open files", factor: 1, resource: unix.RLIMIT_NOFILE},
	// The pipe buffer is not an rlimit and cannot be set; bash reports it
	// in 512-byte units, and linux's 4096-byte buffer is eight of them.
	{letter: 'p', label: "pipe size", unit: "512 bytes", factor: 1, resource: noResource, fixed: 8},
	{letter: 'q', label: "POSIX message queues", unit: "bytes", factor: 1, resource: unix.RLIMIT_MSGQUEUE},
	{letter: 'r', label: "real-time priority", factor: 1, resource: unix.RLIMIT_RTPRIO},
	{letter: 's', label: "stack size", unit: "kbytes", factor: 1024, resource: unix.RLIMIT_STACK},
	{letter: 't', label: "cpu time", unit: "seconds", factor: 1, resource: unix.RLIMIT_CPU},
	{letter: 'u', label: "max user processes", factor: 1, resource: unix.RLIMIT_NPROC},
	{letter: 'v', label: "virtual memory", unit: "kbytes", factor: 1024, resource: unix.RLIMIT_AS},
	{letter: 'x', label: "file locks", factor: 1, resource: unix.RLIMIT_LOCKS},
}
