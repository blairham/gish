//go:build unix

package interp

import (
	"slices"
	"syscall"
)

// The signal table `trap -l` prints (#268).
//
// Deliberately the same portable set internal/jobs uses for `kill -l`,
// and for the reason stated there: a name that means different things on
// different unixes is worse than one that is simply absent. The numbers
// come from the platform rather than a literal list, because they differ
// between Linux and darwin and the number is the part a caller acts on.
//
// This is a second copy of that set, which is a cost worth naming. The
// alternative is an import, and internal/jobs already imports this
// package — so sharing would mean a third package existing only to hold
// twenty constants. TestTrapListMatchesKillList runs both listings
// through a real koi and requires them to agree, which is the thing that
// actually keeps a duplicate honest; a shared variable that nobody
// compares can still be printed two different ways.
var signalTable = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT,
	"ILL": syscall.SIGILL, "TRAP": syscall.SIGTRAP, "ABRT": syscall.SIGABRT,
	"FPE": syscall.SIGFPE, "KILL": syscall.SIGKILL, "SEGV": syscall.SIGSEGV,
	"PIPE": syscall.SIGPIPE, "ALRM": syscall.SIGALRM, "TERM": syscall.SIGTERM,
	"USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "CHLD": syscall.SIGCHLD,
	"CONT": syscall.SIGCONT, "STOP": syscall.SIGSTOP, "TSTP": syscall.SIGTSTP,
	"TTIN": syscall.SIGTTIN, "TTOU": syscall.SIGTTOU, "WINCH": syscall.SIGWINCH,
}

type signalEntry struct {
	name string
	num  int
}

// signalList returns the table in signal-number order, which is the only
// order a numbered listing can sensibly be read in.
func signalList() []signalEntry {
	out := make([]signalEntry, 0, len(signalTable))
	for name, num := range signalTable {
		out = append(out, signalEntry{name: name, num: int(num)})
	}
	slices.SortFunc(out, func(a, b signalEntry) int { return a.num - b.num })
	return out
}
