//go:build unix

package jobs

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// kill (#55).
//
// The interpreter recognizes the name and answers "unsupported builtin",
// which is worse than not having it: because a recognized builtin never
// reaches the exec seam, the working /bin/kill on the machine is
// shadowed by something that refuses to run. `kill %1` and `kill -9 pid`
// both failed, so a job you could stop with Ctrl-Z could not be killed.
//
// It lives here rather than in internal/builtins because the useful half
// is the job specs: `%1` has to resolve through the same table that
// jobs/fg/bg read, and signaling a *job* means signaling its process
// group, not one pid. Plain pids work too, which is what scripts use.
//
// Registered as __koi_kill and reached through the CallHandler rewrite,
// exactly like jobs/fg/bg.

// killSignals maps the names kill accepts to their numbers. Only the
// portable set: a name that means different things on different unixes
// is worse than one that is simply absent.
var killSignals = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT,
	"ILL": syscall.SIGILL, "TRAP": syscall.SIGTRAP, "ABRT": syscall.SIGABRT,
	"FPE": syscall.SIGFPE, "KILL": syscall.SIGKILL, "SEGV": syscall.SIGSEGV,
	"PIPE": syscall.SIGPIPE, "ALRM": syscall.SIGALRM, "TERM": syscall.SIGTERM,
	"USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "CHLD": syscall.SIGCHLD,
	"CONT": syscall.SIGCONT, "STOP": syscall.SIGSTOP, "TSTP": syscall.SIGTSTP,
	"TTIN": syscall.SIGTTIN, "TTOU": syscall.SIGTTOU, "WINCH": syscall.SIGWINCH,
}

const killUsage = `usage: kill [-s SIGNAL | -SIGNAL] pid | %job …
       kill -l [status]`

// Kill signals jobs or processes.
func (t *Table) Kill(_ context.Context, hc interp.HandlerContext, args []string) error {
	sig := syscall.SIGTERM
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		switch {
		case args[0] == "-l" || args[0] == "-L":
			// With operands, -l *translates* rather than listing: a
			// number becomes a name and a name becomes a number, which
			// is how a script turns an exit status above 128 into the
			// signal that caused it. koi dumped the whole table and
			// ignored the arguments (#411).
			if rest := args[1:]; len(rest) > 0 {
				return translateSignals(hc, rest)
			}
			listSignals(hc)
			return nil
		case args[0] == "-s" && len(args) > 1:
			s, ok := parseSignal(args[1])
			if !ok {
				hc.Errf("kill: %s: invalid signal specification\n", args[1])
				return interp.ExitStatus(1)
			}
			sig, args = s, args[2:]
		case args[0] == "--":
			args = args[1:]
			// Everything after -- is a target, even if it looks like a flag.
			goto targets
		default:
			s, ok := parseSignal(strings.TrimPrefix(args[0], "-"))
			if !ok {
				hc.Errf("kill: %s: invalid signal specification\n", args[0])
				hc.RawErrf("%s\n", killUsage)
				return interp.ExitStatus(1)
			}
			sig, args = s, args[1:]
		}
	}

targets:
	if len(args) == 0 {
		// bash prints the usage line bare for a bare `kill` — a usage
		// line is not a diagnostic and carries no location (#611).
		hc.RawErrf("%s\n", killUsage)
		return interp.ExitStatus(2)
	}

	failed := false
	for _, target := range args {
		if err := t.signalTarget(target, sig); err != nil {
			hc.Errf("kill: %s\n", err)
			failed = true
		}
	}
	if failed {
		return interp.ExitStatus(1)
	}
	return nil
}

// signalTarget resolves one operand and signals it.
//
// A %job is signaled by process group, which is the whole point of job
// control: a pipeline is several processes, and killing only the first
// leaves the rest running. A bare pid is signaled as itself, because
// that is what the caller asked for.
func (t *Table) signalTarget(target string, sig syscall.Signal) error {
	if strings.HasPrefix(target, "%") {
		job := t.pick([]string{target})
		if job == nil {
			if t.signalHook != nil {
				// Not this table's job, which in a script means it is
				// the interpreter's (#397).
				return t.signalHook(target, sig)
			}
			return fmt.Errorf("%s: no such job", target)
		}
		if job.Pgid == 0 {
			return fmt.Errorf("%s: job has no process group", target)
		}
		// A stopped process cannot act on anything but SIGCONT and
		// SIGKILL until it runs again, so a plain `kill %1` on a stopped
		// job would leave it stopped and unkilled — the surprise bash
		// avoids by continuing it.
		if s, _ := job.snapshot(); s == Stopped && sig != syscall.SIGKILL && sig != syscall.SIGCONT {
			_ = syscall.Kill(-job.Pgid, syscall.SIGCONT) //nolint:errcheck // best effort
		}
		if err := syscall.Kill(-job.Pgid, sig); err != nil {
			return fmt.Errorf("%s: %v", target, err)
		}
		return nil
	}

	pid, err := strconv.Atoi(target)
	if err != nil {
		return fmt.Errorf("%s: arguments must be process or job IDs", target)
	}
	if err := syscall.Kill(pid, sig); err != nil {
		return fmt.Errorf("(%d): %v", pid, err)
	}
	// A signal the shell sends itself is pending before kill(2) returns,
	// and bash's handler has run before the next command starts — which is
	// what `trap 'echo t' USR1; kill -USR1 $$; echo after` relies on for
	// its order. Go forwards signals to os/signal channels from a separate
	// goroutine, so without a pause the next statement's trap check runs
	// before the signal has crossed (#350). The pause is only for signals
	// aimed at this process; signaling anything else stays instant.
	if pid == os.Getpid() || pid == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// parseSignal accepts a number, a name, or a SIG-prefixed name, in any
// case — every spelling `kill -9`, `kill -KILL`, `kill -sigkill` uses.
func parseSignal(spec string) (syscall.Signal, bool) {
	if spec == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(spec); err == nil {
		if n < 0 || n > 64 {
			return 0, false
		}
		return syscall.Signal(n), true
	}
	name := strings.ToUpper(spec)
	name = strings.TrimPrefix(name, "SIG")
	sig, ok := killSignals[name]
	return sig, ok
}

// listSignals prints the names kill accepts, sorted by number so the
// listing reads like kill -l everywhere else.
// translateSignals implements `kill -l NAME|NUMBER ...`.
func translateSignals(hc interp.HandlerContext, args []string) error {
	failed := false
	for _, arg := range args {
		if n, err := strconv.Atoi(arg); err == nil {
			// A status above 128 is 128+signal, which is the form a
			// caller most often has.
			if n > 128 {
				n -= 128
			}
			if n == 0 {
				fmt.Fprintln(hc.Stdout, "EXIT")
				continue
			}
			name := signalName(syscall.Signal(n))
			if name == "" {
				hc.Errf("kill: %s: invalid signal specification\n", arg)
				failed = true
				continue
			}
			fmt.Fprintln(hc.Stdout, name)
			continue
		}
		sig, ok := parseSignal(arg)
		if !ok {
			hc.Errf("kill: %s: invalid signal specification\n", arg)
			failed = true
			continue
		}
		fmt.Fprintln(hc.Stdout, int(sig))
	}
	if failed {
		return interp.ExitStatus(1)
	}
	return nil
}

// signalName is the reverse of the killSignals table.
func signalName(sig syscall.Signal) string {
	for name, num := range killSignals {
		if num == sig {
			return name
		}
	}
	return ""
}

func listSignals(hc interp.HandlerContext) {
	type entry struct {
		name string
		num  syscall.Signal
	}
	list := make([]entry, 0, len(killSignals))
	for name, num := range killSignals {
		list = append(list, entry{name, num})
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].num < list[j-1].num; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	for _, e := range list {
		fmt.Fprintf(hc.Stdout, "%2d) SIG%s\n", e.num, e.name)
	}
}
