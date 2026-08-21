//go:build unix

package interp

import "syscall"

// jobStopSignal reports whether a signal asks for a job to stop or to
// carry on, which is the one thing [Runner.SignalJob] cannot do: a
// goroutine has no stopped state, and there is no process group to send
// SIGCONT to. The verb is for the message.
func jobStopSignal(sig syscall.Signal) (string, bool) {
	switch sig {
	case syscall.SIGCONT:
		return "continue", true
	case syscall.SIGSTOP, syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU:
		return "stop", true
	}
	return "", false
}
