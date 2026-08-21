package jobs

import "syscall"

// SignalHook answers for a %jobspec this table does not know.
//
// A script's jobs are not in this table: it tracks the process groups an
// interactive line creates, while a script's background commands live in
// the interpreter as goroutines (#397). `kill %1` therefore has two
// possible owners, and which one is right is decided by which one has
// the job — so the table answers first and the hook is the fallback,
// which keeps the interactive meaning of `kill %1` (signal the whole
// process group) exactly as it was.
type SignalHook func(spec string, sig syscall.Signal) error

// SetSignalHook installs that fallback. nil clears it.
func (t *Table) SetSignalHook(fn SignalHook) { t.signalHook = fn }
