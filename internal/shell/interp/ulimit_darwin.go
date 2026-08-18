package interp

import "golang.org/x/sys/unix"

// The rows darwin's bash reports, in its order — which is the option
// letter's byte order, so the list is simply sorted by letter.
//
// The labels and units are bash's own strings, measured from
// `ulimit -a`: a grep for "open files" is a normal way to read this
// output, so paraphrasing them would break callers for no gain.
var ulimitSpecs = []ulimitSpec{
	// "blocks" is bash's word and it means 1024 bytes here, not the 512
	// the name suggests — measured, because a round-trip through one
	// shell cannot tell the two apart and only a second reader of the
	// same kernel value can.
	{letter: 'c', label: "core file size", unit: "blocks", factor: 1024, resource: unix.RLIMIT_CORE},
	{letter: 'd', label: "data seg size", unit: "kbytes", factor: 1024, resource: unix.RLIMIT_DATA},
	{letter: 'f', label: "file size", unit: "blocks", factor: 1024, resource: unix.RLIMIT_FSIZE},
	{letter: 'l', label: "max locked memory", unit: "kbytes", factor: 1024, resource: unix.RLIMIT_MEMLOCK},
	{letter: 'm', label: "max memory size", unit: "kbytes", factor: 1024, resource: unix.RLIMIT_RSS},
	{letter: 'n', label: "open files", factor: 1, resource: unix.RLIMIT_NOFILE},
	// The pipe buffer is not an rlimit and cannot be set; bash reports it
	// in 512-byte units, and darwin's is one of them.
	{letter: 'p', label: "pipe size", unit: "512 bytes", factor: 1, resource: noResource, fixed: 1},
	{letter: 's', label: "stack size", unit: "kbytes", factor: 1024, resource: unix.RLIMIT_STACK},
	{letter: 't', label: "cpu time", unit: "seconds", factor: 1, resource: unix.RLIMIT_CPU},
	{letter: 'u', label: "max user processes", factor: 1, resource: unix.RLIMIT_NPROC},
	{letter: 'v', label: "virtual memory", unit: "kbytes", factor: 1024, resource: unix.RLIMIT_AS},
}
