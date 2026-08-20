package repl

import (
	"os"
	"os/signal"
	"syscall"
)

// probedSignals is HUP and INT, and the short list is the finding
// rather than an oversight.
//
// Reading a disposition needs sigaction, which os/signal does not
// expose; signal.Ignored answers the same question, but only for the
// signals the Go runtime *leaves alone* when it starts. The runtime
// installs its own handler for everything else before main runs — so
// for QUIT, TERM, USR1 and the rest, signal.Ignored answers false
// whatever the parent did, and probing them would add noise rather than
// information. HUP and INT are the two it preserves, and they are also
// the two a parent actually ignores in practice (nohup, and a job
// runner shielding its children).
var probedSignals = []struct {
	name string
	sig  os.Signal
}{
	{"HUP", syscall.SIGHUP},
	{"INT", syscall.SIGINT},
}

// scanIgnoredSignals reports which signals were ignored when the process
// started (#441). POSIX says a non-interactive shell may neither trap
// nor reset one of those, and bash lists them; koi never looked.
//
// It must run before anything installs a handler of its own — which is
// why the caller does it at the top of main — because after that the
// answer describes koi rather than what koi was handed.
func scanIgnoredSignals() []string {
	var ignored []string
	for _, s := range probedSignals {
		if signal.Ignored(s.sig) {
			ignored = append(ignored, s.name)
		}
	}
	return ignored
}
