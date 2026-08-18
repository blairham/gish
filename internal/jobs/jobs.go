// Package jobs implements job control for the interactive shell:
// per-command-line process groups, terminal handoff, Ctrl-Z stop
// detection, and the jobs/fg/bg builtins. Design notes live on issue #5.
//
// The interpreter recognizes jobs/fg/bg as builtins and would reject
// them before koi's builtin seam, so a CallHandler rewrites those names
// to registry-internal ones first (the #18 finding).
package jobs

import "context"

// Builtin names as the user types them → the koi builtin registry names
// they are rewritten to. The dunder names fall through the interpreter's
// builtin dispatch into the ExecHandler seam.
var rewrites = map[string]string{
	"jobs": "__koi_jobs",
	"fg":   "__koi_fg",
	"bg":   "__koi_bg",
}

// RewriteCall is the interp.CallHandlerFunc that reroutes job-control
// builtins. It must be installed only when a Table is serving them.
func RewriteCall(_ context.Context, args []string) ([]string, error) {
	if to, ok := rewrites[args[0]]; ok {
		args[0] = to
	}
	return args, nil
}

// Range is the source span of one backgrounded statement, in byte
// offsets into the command line about to run.
//
// Whether a command was written with & cannot be learned at the exec
// seam by timing: the interpreter runs a background statement on its own
// goroutine, and whether that goroutine arrives before or after the line
// finishes is a race that resolves differently per platform. Position is
// not a race — the shell has already parsed the line, and
// interp.HandlerContext carries the position of every call it makes.
type Range struct{ Start, End uint }

// State is a job's lifecycle position.
type State int

const (
	Running State = iota
	Stopped
	Done
)

func (s State) String() string {
	switch s {
	case Running:
		return "Running"
	case Stopped:
		return "Stopped"
	default:
		return "Done"
	}
}

// Notice reports what EndLine filed, for the shell to print.
type Notice struct {
	ID      int
	Command string
	Stopped bool
}
