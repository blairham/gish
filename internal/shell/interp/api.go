// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

// Package interp implements an interpreter to execute shell programs
// parsed by the [syntax] package as either [syntax.LangBash]
// or [syntax.LangPOSIX], behaving like Bash as a result.
//
// The interpreter currently aims to behave like a non-interactive shell,
// which is how most shells run scripts, and is more useful to machines.
// In the future, it may gain an option to behave like an interactive shell.
package interp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"mvdan.cc/sh/v3/syntax"
)

// A Runner interprets shell programs. It can be reused, but it is not safe for
// concurrent use. Use [New] to build a new Runner.
//
// Note that writes to Stdout and Stderr may be concurrent if background
// commands are used. If you plan on using an [io.Writer] implementation that
// isn't safe for concurrent use, consider a workaround like hiding writes
// behind a mutex.
//
// Runner's exported fields are meant to be configured via [RunnerOption];
// once a Runner has been created, the fields should be treated as read-only.
type Runner struct {
	// Env specifies the initial environment for the interpreter, which must
	// not be nil. It can only be set via [Env].
	//
	// If it includes a TMPDIR variable describing an absolute directory,
	// it is used as the directory in which to create temporary files needed
	// for the interpreter's use, such as named pipes for process substitutions.
	// Otherwise, [os.TempDir] is used.
	Env expand.Environ

	// writeEnv overlays [Runner.Env] so that we can write environment variables
	// as an overlay.
	writeEnv expand.WriteEnviron

	// Dir specifies the working directory of the command, which must be an
	// absolute path. It can only be set via [Dir].
	Dir string

	// tempDir is either $TMPDIR from [Runner.Env], or [os.TempDir].
	tempDir string

	// Params are the current shell parameters, e.g. from running a shell
	// file or calling a function. Accessible via the $@/$* family of vars.
	// It can only be set via [Params].
	Params []string

	// Separate maps - note that bash allows a name to be both a var and a
	// func simultaneously.
	// Vars is mostly superseded by Env at this point.
	// TODO(v4): remove these

	Vars  map[string]expand.Variable
	Funcs map[string]*syntax.Stmt

	alias map[string]alias

	// callHandler is a function allowing to replace a simple command's
	// arguments. It may be nil.
	callHandler CallHandlerFunc

	// execHandler is responsible for executing programs. It must not be nil.
	execHandler ExecHandlerFunc

	// execMiddlewares grows with calls to [ExecHandlers],
	// and is used to construct execHandler when Reset is first called.
	// The slice is needed to preserve the relative order of middlewares.
	execMiddlewares []func(ExecHandlerFunc) ExecHandlerFunc

	// openHandler is a function responsible for opening files. It must not be nil.
	openHandler OpenHandlerFunc

	// readDirHandler is a function responsible for reading directories during
	// glob expansion. It must be non-nil.
	readDirHandler ReadDirHandlerFunc2

	// statHandler is a function responsible for getting file stat. It must be non-nil.
	statHandler StatHandlerFunc

	// accessHandler is a function responsible for checking file access. It must be non-nil.
	accessHandler AccessHandlerFunc

	stdin  *os.File // e.g. the read end of a pipe
	stdout io.Writer
	stderr io.Writer

	// bgWriteMu serializes writes to stdout and stderr once a background
	// job exists to share them. It is a pointer so that subshells hold the
	// same lock as the shell that spawned them; see bgwrite.go.
	bgWriteMu *sync.Mutex

	ecfg *expand.Config
	ectx context.Context // just so that Runner.Subshell can use it again

	// didReset remembers whether the runner has ever been reset. This is
	// used so that Reset is automatically called when running any program
	// or node for the first time on a Runner.
	didReset bool

	usedNew bool

	filename string // only if Node was a File

	// >0 to break or continue out of N enclosing loops
	breakEnclosing, contnEnclosing int

	inLoop       bool
	inFunc       bool
	inSource     bool
	handlingTrap bool // whether we're currently in a trap callback

	// track if a sourced script set positional parameters
	sourceSetParams bool

	// declTempNames, when non-nil, records the names a declaration
	// builtin declared while a temp-env prefix assignment was in flight
	// (#380): `var=value declare -x var`. The value is whether the
	// declaration created a function-local, which decides how the temp
	// binding is unwound afterwards.
	declTempNames map[string]bool

	// noErrExit prevents failing commands from triggering [optErrExit],
	// such as the condition in a [syntax.IfClause].
	noErrExit bool

	// The current and last exit statuses. They can only be different if
	// the interpreter is in the middle of running a statement. In that
	// scenario, 'exit' is the status for the current statement being run,
	// and 'lastExit' corresponds to the previous statement that was run.
	exit     exitStatus
	lastExit exitStatus

	lastExpandExit exitStatus // used to surface exit statuses while expanding fields

	// bgProcs holds all background shells spawned by this runner.
	// Their PIDs are 1-indexed, from 1 to len(bgProcs), with a "g" prefix
	// to distinguish them from real PIDs on the host operating system.
	//
	// Note that each shell only tracks its direct children;
	// subshells do not share nor inherit the background PIDs they can wait for.
	bgProcs []bgProc

	opts runnerOpts

	origDir    string
	origParams []string
	origOpts   runnerOpts
	origStdin  *os.File
	origStdout io.Writer
	origStderr io.Writer

	// Most scripts don't use pushd/popd, so make space for the initial PWD
	// without requiring an extra allocation.
	dirStack     []string
	dirBootstrap [1]string

	optState getopts

	// keepRedirs is set by "exec" so that its statement's redirections
	// apply to the current shell, and not just the command.
	// It is consumed by the enclosing statement once it finishes.
	keepRedirs bool

	// Fake signal callbacks
	callbackErr    string
	callbackExit   string
	callbackDebug  string
	callbackReturn string

	// Real-signal traps (#350). sigTraps holds what runs, keyed by the
	// signal table's bare name; an empty action means the signal is
	// ignored. sigListed mirrors it for `trap -p` and, like listed, is
	// inherited by subshells unconditionally, while the handlers follow
	// bash's rule and are not: a subshell resets signal traps to their
	// defaults. The channel is armed lazily on the first handler and
	// shared by every signal, with sigNames mapping a delivery back to
	// its name; it is drained at statement boundaries, which is bash's
	// granularity — a signal arriving mid-command runs its trap after
	// that command, before the next one.
	sigTraps  map[string]string
	sigListed map[string]string
	sigChan   chan os.Signal
	sigNames  map[os.Signal]string

	// Where each non-command trap was set, for $LINENO inside its action
	// (#352): bash counts an EXIT, RETURN, or signal trap's action lines
	// from the line of the `trap` command that installed it, where DEBUG
	// and ERR count from the line of the command that triggered them.
	callbackExitLine   uint
	callbackReturnLine uint
	sigTrapLines       map[string]uint

	// exitTrapFired notes the EXIT trap already ran: `exit` inside a
	// function fires it early, at the call, so the action still sees
	// that function's FUNCNAME and locals (#352) — by the time [Runner.Run]'s
	// own firing point is reached the frames are gone.
	exitTrapFired bool

	// returnTrapOff disables the RETURN trap for the frame being run,
	// which is the whole of how bash's inheritance rule works (#295).
	//
	// The handler itself is global — `trap ... RETURN` sets one, and
	// nothing puts it back afterwards, which is why a trap set inside a
	// function is still listed by `trap -p` once that function has
	// returned, and still fires for a later top-level `source`. What is
	// per-frame is only whether it is *reachable*: entering a function
	// turns it off unless "functrace" is set, and leaving restores what
	// the caller had. Entering a `source` does not turn it off at all.
	//
	// The two halves have to be separate. Saving and restoring the
	// handler would lose a trap the function set; not restoring the flag
	// would let a nested call's disable leak back into its caller and
	// silence the caller's own return. Both were measured against bash
	// before being written down.
	returnTrapOff bool

	// listed mirrors the callbacks above for `trap -p`'s benefit, and is
	// inherited by a subshell unconditionally where they are not.
	//
	// The two answer different questions. Whether a handler *runs* in a
	// subshell follows bash's inheritance rules, and getting that wrong
	// means an EXIT trap firing once per subshell. Whether `trap -p`
	// *reports* it does not: `saved=$(trap -p EXIT)` is the documented way
	// to save a handler, it runs in a command substitution, and reporting
	// the subshell's empty set there hands back an empty string — so the
	// restore installs nothing and the handler is silently lost, which is
	// worse than `trap -p` never having worked.
	listed listedTraps

	// errTrapFired records that the ERR trap has already run for the failure
	// which the current status describes, so that the compound commands
	// propagating that status outwards do not run it again. It is per level:
	// entering a function or a subshell saves and clears it, because bash runs
	// the trap once more for the call itself.
	errTrapFired bool

	// errTrapDepth counts the function calls and subshells we are inside of.
	// The ERR trap only runs below the top level when "errtrace" is set, which
	// is what bash's -E option means.
	errTrapDepth int

	// extraFiles holds what is open on the file descriptors above 2, keyed by
	// descriptor number. The shell uses them for its own redirections and
	// passes them on to the commands it runs.
	extraFiles map[int]io.ReadWriteCloser

	// frames is the execution-context stack, innermost first: one entry per
	// function call, one per `source`, and one for the script itself. It is
	// what FUNCNAME, BASH_SOURCE, BASH_LINENO and `caller` all read (#266).
	//
	// A stack of names was enough for FUNCNAME alone. The other three need
	// a file and a line per frame, and a frame for `source` as well as for
	// a call, which is why this replaced it rather than growing beside it.
	frames []callFrame

	// mainScript is the script file this runner was started on, or empty
	// for a command string. It decides whether the bottom frame exists at
	// all: bash gives `-c` no `main` frame, so `bash -c 'f(){ …; }; f'`
	// reports one context where a script would report two.
	mainScript string

	// funcSource records the file each function was defined in, which is
	// what BASH_SOURCE reports for its frame — not the file it is called
	// from. A function defined in a sourced library and called from the
	// main script names the library, which is the whole point of the
	// variable for a logging helper.
	funcSource map[string]string

	// historyHook is called for each top-level statement about to run
	// while `set -o history` is on — the recording half of bash's
	// ambient history (#277). The interpreter fires it and owns nothing
	// else: rendering the entry text, HISTCONTROL/HISTIGNORE/HISTSIZE,
	// and the list itself all belong to the shell around it, which is
	// the side that has the raw source and the `history` builtin.
	//
	// It deliberately does not fire for `source` or `eval`, whose
	// statements never pass through stmtsTopLevel — bash records the
	// sourcing line, not the sourced file's contents.
	historyHook func(*syntax.Stmt)

	// traceHook is called with a [TraceEvent] after each simple command
	// the runner executes (#474). Unlike historyHook it follows execution
	// everywhere — functions, subshells, pipeline stages, `source` and
	// `eval` — because a trace that skipped the sourced file would miss
	// the command that failed. Pipeline stages run concurrently, so the
	// hook must be safe for concurrent use.
	traceHook func(TraceEvent)

	// pipeStatus collects the exit status of each stage of the pipeline being
	// run, left to right, for [shellPipeStatusVar]. It is nil when the command
	// is not a pipeline, in which case that variable holds just the one status.
	pipeStatus []uint8

	// pipeStatusSet records that [shellPipeStatusVar] has already been set for
	// the command which produced the current status. Like errTrapFired it is
	// per level: a block or an "if" does not refresh it, whereas a function
	// call or a subshell counts as one command and does.
	pipeStatusSet bool
}

// exitStatus holds the state of the shell after running one command.
// Beyond the exit status code, it also holds whether the shell should return or exit,
// as well as any Go error values that should be given back to the user.
//
// TODO(v4): consider replacing ExitStatus with a struct like this,
// so that an [ExecHandlerFunc] can e.g. mimic `exit 0` or fatal errors
// with specific exit codes.
type exitStatus struct {
	// code is the exit status code.
	// When code is zero, err must be nil.
	code uint8

	// TODO: consider an enum, as only one of these should be set at a time
	returning bool // whether the current function `return`ed
	exiting   bool // whether the current shell is exiting
	fatalExit bool // whether the current shell is exiting due to a fatal error; err below must not be nil

	// aborting unwinds to the top level and resumes there, which is what
	// bash does to the errors it calls fatal in a non-interactive shell --
	// an assignment to a readonly variable being the one koi raises (#308).
	// It is not `exiting`: the shell survives and reads on. See
	// [Runner.stmtsTopLevel] for where it is caught and why the resuming
	// point is a line rather than a statement.
	aborting bool

	// err holds the error information for a non-zero exit status code or fatal error.
	// Used so that running a single statement with a custom handler
	// which returns a non-fatal Go error, such as a Go error wrapping [NewExitStatus],
	// can be returned by [Runner.Run] without being lost entirely.
	err error
}

// clear sets the exit status code and error to zero, as long as the exit status
// was not set by `return`, `exit`, or a fatal error.
func (e *exitStatus) clear() {
	if e.returning || e.exiting || e.fatalExit || e.aborting {
		return
	}
	e.code = 0
	e.err = nil
}

func (e *exitStatus) ok() bool { return e.code == 0 }

// oneIf sets the exit status code to 1 if b is true.
// Note that it assumes the exit status hasn't been set yet,
// meaning that [exitStatus.code] and [exitStatus.err] are zero values.
func (e *exitStatus) oneIf(b bool) {
	if b {
		e.code = 1
	}
}

func (e *exitStatus) fatal(err error) {
	if e.fatalExit || err == nil {
		return
	}
	e.exiting = true
	e.fatalExit = true
	e.err = err
	if e.code == 0 {
		e.code = 1
	}
}

func (e *exitStatus) fromHandlerError(err error) {
	if err == nil {
		return
	}
	var exit errBuiltinExitStatus
	var es ExitStatus
	if errors.As(err, &exit) {
		*e = exitStatus(exit)
	} else if errors.As(err, &es) {
		e.err = err
		e.code = uint8(es)
	} else {
		e.fatal(err) // handler's custom fatal error
	}
}

type bgProc struct {
	// closed when the background process finishes,
	// after which point the result fields below are set.
	done chan struct{}

	exit *exitStatus

	// reaped records that a wait has already collected this job, which is
	// what makes `wait -n` return each job once and then answer 127 (#287).
	// bash reaps in every form of wait, not only -n, so plain `wait` and
	// `wait PID` set it too — otherwise `wait; wait -n` would hand back a
	// job bash considers long gone.
	reaped bool

	// cmd is the job's command as written, which is the only thing `jobs`
	// has to show for it: koi's jobs are goroutines, so there is no
	// process to ask afterwards what it was running.
	cmd string

	// inherited marks a job this shell can *see* but is not the parent of.
	// A command substitution gets one of these per job of the shell that
	// spawned it, because `jobs` inside `$(...)` reports the caller's jobs
	// in bash -- which is the whole reason the bounded parallel loop's
	// `$(jobs -r | wc -l)` counts anything at all. `wait` still refuses
	// them, exactly as bash's does: "not a child of this shell".
	inherited bool

	// reported records that a finished job has already been listed. bash
	// mentions a job's completion once and then drops it from the table,
	// so a second `jobs` in the same script does not report the same Done
	// job again.
	reported bool
}

type alias struct {
	args  []*syntax.Word
	blank bool
}

// New creates a new Runner, applying a number of options. If applying any of
// the options results in an error, it is returned.
//
// Any unset options fall back to their defaults. For example, not supplying the
// environment falls back to the process's environment, and not supplying the
// standard output writer means that the output will be discarded.
func New(opts ...RunnerOption) (*Runner, error) {
	r := &Runner{
		usedNew:        true,
		openHandler:    DefaultOpenHandler(),
		readDirHandler: DefaultReadDirHandler2(),
		statHandler:    DefaultStatHandler(),
		accessHandler:  DefaultAccessHandler(),
	}
	r.dirStack = r.dirBootstrap[:0]
	// turn "on" the options bash starts with on — braceexpand, hashall and
	// interactive-comments among the `set -o` ones, so that `set -o`
	// reports what bash reports rather than a table of zeroes.
	for i, opt := range &posixOptsTable {
		r.opts[i] = opt.defaultState
	}
	for i, opt := range bashOptsTable {
		r.opts[len(posixOptsTable)+i] = opt.defaultState
	}

	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, err
		}
	}

	// Set the default fallbacks, if necessary.
	if r.Env == nil {
		Env(nil)(r)
	}
	if r.Dir == "" {
		if err := Dir("")(r); err != nil {
			return nil, err
		}
	}
	if r.stdout == nil || r.stderr == nil {
		StdIO(r.stdin, r.stdout, r.stderr)(r)
	}
	return r, nil
}

// RunnerOption can be passed to [New] to alter a [Runner]'s behaviour.
// It can also be applied directly on an existing Runner,
// such as interp.Params("-e")(runner).
// Note that options cannot be applied once Run or Reset have been called.
type RunnerOption func(*Runner) error

// TODO: enforce the rule above via didReset.

// Env sets the interpreter's environment. If nil, a copy of the current
// process's environment is used.
func Env(env expand.Environ) RunnerOption {
	return func(r *Runner) error {
		if env == nil {
			env = expand.ListEnviron(os.Environ()...)
		}
		r.Env = env
		return nil
	}
}

// Dir sets the interpreter's working directory. If empty, the process's current
// directory is used.
func Dir(path string) RunnerOption {
	return func(r *Runner) error {
		if path == "" {
			path, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("could not get current dir: %w", err)
			}
			r.Dir = path
			return nil
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("could not get absolute dir: %w", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("could not stat: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		r.Dir = path
		return nil
	}
}

// Interactive configures the interpreter to behave like an interactive shell,
// akin to Bash. Currently, this only enables the expansion of aliases,
// but later on it should also change other behavior.
func Interactive(enabled bool) RunnerOption {
	return func(r *Runner) error {
		r.opts[optExpandAliases] = enabled
		return nil
	}
}

// MainScript declares that this runner is executing the named script file
// rather than a command string, which is the difference bash reports in
// FUNCNAME: `bash -c 'f(){ …; }; f'` names one frame where the same code in
// a file names two, the second being `main` (#266).
//
// It is separate from the parse name because that cannot answer the
// question — koi hands the parser "koi" for a `-c` session, since the parse
// name is also what $0 reports.
func MainScript(path string) RunnerOption {
	return func(r *Runner) error {
		r.mainScript = path
		return nil
	}
}

// LookupVar reads a variable as the running script sees it *now*,
// including values assigned mid-run — which [Runner.Vars] only reflects
// once Run returns, because the live values sit in an overlay until then.
// The ambient-history recorder reads HISTCONTROL, HISTIGNORE and HISTSIZE
// through this at record time (#277), since a script sets them as it goes.
func (r *Runner) LookupVar(name string) expand.Variable {
	if r.writeEnv != nil {
		return r.writeEnv.Get(name)
	}
	return r.Env.Get(name)
}

// Aliases returns the session's alias definitions rendered back to
// source, name → replacement text (#473). The interpreter records an
// alias as parsed words, so the text is a print of those words — which
// is what `alias` itself would show — with the trailing space that marks
// a blank-continuing alias preserved, since it changes what the alias
// means.
func (r *Runner) Aliases() map[string]string {
	out := make(map[string]string, len(r.alias))
	printer := syntax.NewPrinter()
	for name, als := range r.alias {
		var sb strings.Builder
		for i, w := range als.args {
			if i > 0 {
				sb.WriteString(" ")
			}
			_ = printer.Print(&sb, w)
		}
		if als.blank {
			sb.WriteString(" ")
		}
		out[name] = sb.String()
	}
	return out
}

// DirStack returns a copy of the pushd/popd stack, top (the current
// directory) first — the order `dirs` prints.
func (r *Runner) DirStack() []string {
	out := make([]string, 0, len(r.dirStack))
	for _, dir := range slices.Backward(r.dirStack) {
		out = append(out, dir)
	}
	return out
}

// OptionState is one named shell option and whether it is currently on.
type OptionState struct {
	Name string
	Set  bool
}

// Options lists every supported named shell option — the `set -o` and
// `shopt` tables both — with its live state (#473). Unsupported rows are
// skipped: an option that can never leave its default is a fact about
// koi, not about this session.
func (r *Runner) Options() []OptionState {
	out := make([]OptionState, 0, len(r.opts))
	for i, opt := range posixOptsTable {
		if opt.supported {
			out = append(out, OptionState{opt.name, r.opts[i]})
		}
	}
	for i, opt := range bashOptsTable {
		if opt.supported {
			out = append(out, OptionState{opt.name, r.opts[len(posixOptsTable)+i]})
		}
	}
	return out
}

// HistoryHook installs fn to be called with each top-level statement the
// runner is about to execute while `set -o history` is on (#277). It fires
// *before* the statement runs, which is bash's order — `history` lists
// itself, and the `set +o history` that turns recording off is the last
// line recorded. The statement is handed over rather than rendered text
// because bash records raw source lines, not a pretty-printing: the caller
// holds the source bytes and slices them by the statement's position.
func HistoryHook(fn func(*syntax.Stmt)) RunnerOption {
	return func(r *Runner) error {
		r.historyHook = fn
		return nil
	}
}

// TraceEvent describes one simple command the runner executed, handed to
// the hook installed by [TraceHook] after the command returns. The
// timing/exit field names match the shell's history JSONL entries
// (started_unix_ms, duration_ms, and so on) so the two streams join.
type TraceEvent struct {
	// Src is the file the command's line lives in — the innermost
	// source/function frame's file, or the parse name at the top level.
	Src string `json:"src,omitempty"`
	// Line and Col are the command's parsed position within Src.
	Line uint `json:"line"`
	Col  uint `json:"col,omitempty"`
	// Cmd is the command as written, printed from the parse tree before
	// expansion — `$URL` stays `$URL`, which is what makes a trace
	// greppable against the script that produced it.
	Cmd string `json:"cmd"`
	// Expanded is the argv the command actually ran with.
	Expanded []string `json:"expanded"`
	// Func names the enclosing function, when the command ran inside one.
	Func          string `json:"func,omitempty"`
	Exit          int    `json:"exit"`
	StartedUnixMs int64  `json:"started_unix_ms"`
	DurationMs    int64  `json:"duration_ms"`
}

// TraceHook installs fn to be called after every simple command the
// runner executes, with its position, unexpanded text, expanded argv,
// exit status and duration (#474). It is independent of `set -x` and of
// every other shell option: nothing a script does can turn it on or off,
// which is what lets the shell offer tracing that is invisible to
// bash-compatible scripts. fn may be called from concurrent pipeline
// stages and must be safe for that.
func TraceHook(fn func(TraceEvent)) RunnerOption {
	return func(r *Runner) error {
		r.traceHook = fn
		return nil
	}
}

// Params populates the shell options and parameters. For example, Params("-e",
// "--", "foo") will set the "-e" option and the parameters ["foo"], and
// Params("+e") will unset the "-e" option and leave the parameters untouched.
//
// This is similar to what the interpreter's "set" builtin does.
func Params(args ...string) RunnerOption {
	return func(r *Runner) error {
		fp := flagParser{remaining: args}
		for fp.more() {
			flag := fp.flag()
			if flag == "-" || flag == "+" {
				// TODO: for "-", implement "The -x and -v options are turned off."
				if args := fp.args(); len(args) > 0 {
					r.Params = args
				}
				return nil
			}
			enable := flag[0] == '-'
			if flag[1] != 'o' {
				status, opt := r.posixOptByFlag(flag[1])
				if status == nil {
					return fmt.Errorf("invalid option: %q", flag)
				}
				if err := setPosixOpt(status, opt, enable); err != nil {
					return err
				}
				continue
			}
			value := fp.value()
			if value == "" && enable {
				for _, i := range posixOptNames() {
					r.printOptLine(posixOptsTable[i].name, setOptColumn, r.opts[i], true)
				}
				continue
			}
			if value == "" && !enable {
				for _, i := range posixOptNames() {
					setFlag := "+o"
					if r.opts[i] {
						setFlag = "-o"
					}
					r.outf("set %s %s\n", setFlag, posixOptsTable[i].name)
				}
				continue
			}
			status, opt := r.posixOptByName(value)
			if status == nil {
				return fmt.Errorf("invalid option: %q", value)
			}
			if err := setPosixOpt(status, opt, enable); err != nil {
				return err
			}
		}
		if args := fp.args(); args != nil {
			// If "--" wasn't given and there were zero arguments,
			// we don't want to override the current parameters.
			r.Params = args

			// Record whether a sourced script sets the parameters.
			if r.inSource {
				r.sourceSetParams = true
			}
		}
		return nil
	}
}

// CallHandler sets the call handler. See [CallHandlerFunc] for more info.
func CallHandler(f CallHandlerFunc) RunnerOption {
	return func(r *Runner) error {
		r.callHandler = f
		return nil
	}
}

// ExecHandler sets one command execution handler,
// which replaces [DefaultExecHandler](2 * time.Second).
//
// Deprecated: use [ExecHandlers] instead, which allows chaining handlers more easily
// like middleware functions.
func ExecHandler(f ExecHandlerFunc) RunnerOption {
	return func(r *Runner) error {
		r.execHandler = f
		return nil
	}
}

// ExecHandlers appends middlewares to handle command execution.
// The middlewares are chained from first to last, and the first is called by the runner.
// Each middleware is expected to call the "next" middleware at most once.
//
// For example, a middleware may implement only some commands.
// For those commands, it can run its logic and avoid calling "next".
// For any other commands, it can call "next" with the original parameters.
//
// Another common example is a middleware which always calls "next",
// but runs custom logic either before or after that call.
// For instance, a middleware could change the arguments to the "next" call,
// or it could print log lines before or after the call to "next".
//
// The last exec handler is always [DefaultExecHandler](2 * time.Second).
func ExecHandlers(middlewares ...func(next ExecHandlerFunc) ExecHandlerFunc) RunnerOption {
	return func(r *Runner) error {
		r.execMiddlewares = append(r.execMiddlewares, middlewares...)
		return nil
	}
}

// TODO: consider porting the middleware API in [ExecHandlers] to [OpenHandler],
// [ReadDirHandler2], and [StatHandler].

// TODO(v4): now that [ExecHandlers] allows calling a next handler with changed
// arguments, one of the two advantages of [CallHandler] is gone. The other is the
// ability to work with builtins; if we make [ExecHandlers] work with builtins, we
// could join both APIs.

// OpenHandler sets file open handler. See [OpenHandlerFunc] for more info.
func OpenHandler(f OpenHandlerFunc) RunnerOption {
	return func(r *Runner) error {
		r.openHandler = f
		return nil
	}
}

// ReadDirHandler sets the read directory handler. See [ReadDirHandlerFunc] for more info.
//
// Deprecated: use [ReadDirHandler2].
func ReadDirHandler(f ReadDirHandlerFunc) RunnerOption {
	return func(r *Runner) error {
		r.readDirHandler = func(ctx context.Context, path string) ([]fs.DirEntry, error) {
			infos, err := f(ctx, path)
			if err != nil {
				return nil, err
			}
			entries := make([]fs.DirEntry, len(infos))
			for i, info := range infos {
				entries[i] = fs.FileInfoToDirEntry(info)
			}
			return entries, nil
		}
		return nil
	}
}

// ReadDirHandler2 sets the read directory handler. See [ReadDirHandlerFunc2] for more info.
func ReadDirHandler2(f ReadDirHandlerFunc2) RunnerOption {
	return func(r *Runner) error {
		r.readDirHandler = f
		return nil
	}
}

// StatHandler sets the stat handler. See [StatHandlerFunc] for more info.
func StatHandler(f StatHandlerFunc) RunnerOption {
	return func(r *Runner) error {
		r.statHandler = f
		return nil
	}
}

// AccessHandler sets the file access handler. See [AccessHandlerFunc] for more info.
func AccessHandler(f AccessHandlerFunc) RunnerOption {
	return func(r *Runner) error {
		r.accessHandler = f
		return nil
	}
}

func stdinFile(r io.Reader) (*os.File, error) {
	switch r := r.(type) {
	case *os.File:
		return r, nil
	case nil:
		return nil, nil
	default:
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		go func() {
			io.Copy(pw, r)
			pw.Close()
		}()
		return pr, nil
	}
}

// StdIO configures an interpreter's standard input, standard output, and
// standard error. If out or err are nil, they default to a writer that discards
// the output.
//
// Note that providing a non-nil standard input other than [*os.File] will require
// an [os.Pipe] and spawning a goroutine to copy into it,
// as an [os.File] is the only way to share a reader with subprocesses.
// This may cause the interpreter to consume the entire reader.
// See [os/exec.Cmd.Stdin].
//
// When providing an [*os.File] as standard input, consider using an [os.Pipe]
// as it has the best chance to support cancellable reads via [os.File.SetReadDeadline],
// so that cancelling the runner's context can stop a blocked standard input read.
func StdIO(in io.Reader, out, err io.Writer) RunnerOption {
	return func(r *Runner) error {
		stdin, _err := stdinFile(in)
		if _err != nil {
			return _err
		}
		r.stdin = stdin
		if out == nil {
			out = io.Discard
		}
		r.stdout = out
		if err == nil {
			err = io.Discard
		}
		r.stderr = err
		return nil
	}
}

func (r *Runner) posixOptByName(name string) (*bool, posixOpt) {
	for i, opt := range &posixOptsTable {
		if opt.name == name {
			return &r.opts[i], opt
		}
	}
	return nil, posixOpt{}
}

func (r *Runner) posixOptByFlag(flag byte) (*bool, posixOpt) {
	for i, opt := range &posixOptsTable {
		if opt.flag == flag && opt.flag != ' ' {
			return &r.opts[i], opt
		}
	}
	return nil, posixOpt{}
}

// setPosixOpt moves an option, or explains why it cannot.
//
// An option koi does not implement can still be *set to the state it is
// already in* — `set -h` in a shell whose hashall is on, `set +H` in one
// whose histexpand is off. That is not a pretence: nothing changes because
// nothing needs to, which is exactly what bash does with the same line.
// Asking for the other state is refused, because the alternative is a shell
// that says it is in POSIX mode and is not.
func setPosixOpt(status *bool, opt posixOpt, enable bool) error {
	if opt.supported || enable == *status {
		*status = enable
		return nil
	}
	state := "off"
	if enable {
		state = "on"
	}
	return fmt.Errorf("cannot turn %s %s: not implemented", opt.name, state)
}

// posixOptNames lists the options the way bash prints them, which is by
// name rather than in the order the table happens to be in.
func posixOptNames() []int {
	idx := make([]int, len(posixOptsTable))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int {
		return strings.Compare(posixOptsTable[a].name, posixOptsTable[b].name)
	})
	return idx
}

func (r *Runner) bashOptByName(name string) (status *bool, supported bool) {
	for i, opt := range bashOptsTable {
		if opt.name == name {
			index := len(posixOptsTable) + i
			return &r.opts[index], opt.supported
		}
	}
	return nil, false
}

// runnerOpts contains all POSIX Shell and Bash options as one contiguous table.
type runnerOpts [len(posixOptsTable) + len(bashOptsTable)]bool

// posixOpt is one `set -o` option: the letter `set -x` spells it with, the
// name `set -o xtrace` spells it with, the state bash starts it in, and
// whether koi can actually put it in the other state.
//
// The last two are why bash's whole list lives here rather than only the
// options koi implements (#245). `set -o` is a listing people read and
// scripts grep, and koi's answered with ten entries where bash answers with
// twenty-seven. Worse, `set -h` and `set +H` — a no-op in any non-interactive
// bash, since those are already the states it starts in — came back "invalid
// option" and exit 2, which under `set -e` is the end of the script.
//
// An option koi cannot honor is still refused when something asks for the
// state it cannot produce. That is deliberate: the alternative is accepting
// `set -o posix` and not being in POSIX mode, which is the shape of bug this
// issue was opened about rather than a fix for it.
type posixOpt struct {
	flag         byte   // one-character flag form for this option; a space if none exists
	name         string // full name of the option
	defaultState bool   // the state bash starts this option in, non-interactively
	supported    bool   // whether koi can put it in the other state
}

type bashOpt struct {
	name         string
	defaultState bool // Bash's default value for this option
	supported    bool // whether we support the option's non-default state
}

// The order here is the order of the opt* constants below, which index into
// it — not the order `set -o` prints, which is sorted by name at the point
// of printing the way bash's is.
var posixOptsTable = [...]posixOpt{
	// Implemented.
	{'a', "allexport", false, true},
	{'e', "errexit", false, true},
	{'E', "errtrace", false, true},
	{'T', "functrace", false, true},
	{'C', "noclobber", false, true},
	{'n', "noexec", false, true},
	{'f', "noglob", false, true},
	{'u', "nounset", false, true},
	{'x', "xtrace", false, true},
	{' ', "pipefail", false, true},
	{'B', "braceexpand", true, true},
	{'P', "physical", false, true},

	// Known and listed, but koi cannot leave the state bash starts them
	// in. Asking for that state is answered rather than pretended.
	//
	// notify and monitor are job control (#5). histexpand is the line
	// editor's, not the interpreter's, and is already off in a
	// non-interactive shell — which is the state scripts ask for. emacs
	// and vi are the editor's for the same reason. verbose would have to
	// echo input as it is read, which the interpreter never sees: it is
	// handed statements, not lines. posix changes behavior across the
	// whole interpreter and is its own piece of work, named by #308 for
	// the one it already owes. The rest are POSIX corners nothing in the
	// corpus reaches for.
	//
	// history is supported (#277): turning it on makes stmtsTopLevel fire
	// the historyHook per statement, which is ambient recording — the
	// shell around the interpreter renders and keeps the entries. The row
	// stays in this block only because the table is positional (the opt*
	// constants below index into it) and moving it would renumber them.
	{'b', "notify", false, false},
	{'m', "monitor", false, false},
	{'H', "histexpand", false, false},
	{' ', "history", false, true},
	{' ', "emacs", false, false},
	{' ', "vi", false, false},
	{'v', "verbose", false, false},
	{' ', "posix", false, false},
	{'h', "hashall", true, false},
	{' ', "interactive-comments", true, false},
	{'k', "keyword", false, false},
	{'t', "onecmd", false, false},
	{'p', "privileged", false, false},
	{' ', "ignoreeof", false, false},
	{' ', "nolog", false, false},
}

var bashOptsTable = [...]bashOpt{
	// supported options, sorted alphabetically by name
	{
		name:         "dotglob",
		defaultState: false,
		supported:    true,
	},
	{
		name:         "expand_aliases",
		defaultState: false,
		supported:    true,
	},
	{
		name:         "extdebug",
		defaultState: false,
		supported:    true,
	},
	{
		name:         "extglob",
		defaultState: false,
		supported:    true,
	},
	{
		name:         "failglob",
		defaultState: false,
		supported:    true,
	},
	{
		name:         "globstar",
		defaultState: false,
		supported:    true,
	},
	{
		// Off by default, as in bash — and until #277 it was effectively
		// always on: the last pipeline stage ran in the current shell, so
		// `cmd | read x` kept x and `cat f | while read l; do n=$((n+1));
		// done` kept n, which is the single most famous bash gotcha
		// answered un-bash-ly. bash additionally requires job control to
		// be inactive for lastpipe to take effect; the interpreter never
		// has job control (monitor is unsupported here — the shell around
		// it owns jobs), so the option is honored whenever set.
		name:         "lastpipe",
		defaultState: false,
		supported:    true,
	},
	{
		name:         "nocaseglob",
		defaultState: false,
		supported:    true,
	},
	{
		name:         "nullglob",
		defaultState: false,
		supported:    true,
	},
	// unsupported options, sorted alphabetically by name
	{name: "assoc_expand_once"},
	{name: "autocd"},
	{name: "cdable_vars"},
	{name: "cdspell"},
	{name: "checkhash"},
	{name: "checkjobs"},
	{
		name:         "checkwinsize",
		defaultState: true,
	},
	{
		name:         "cmdhist",
		defaultState: true,
	},
	{name: "compat31"},
	{name: "compat32"},
	{name: "compat40"},
	{name: "compat41"},
	{name: "compat42"},
	{name: "compat43"},
	{name: "compat44"},
	{
		name:         "complete_fullquote",
		defaultState: true,
	},
	{name: "direxpand"},
	{name: "dirspell"},
	{name: "execfail"},
	{
		name:         "extquote",
		defaultState: true,
	},
	{
		name:         "force_fignore",
		defaultState: true,
	},
	{name: "globasciiranges"},
	{name: "gnu_errfmt"},
	{name: "histappend"},
	{name: "histreedit"},
	{name: "histverify"},
	{
		name:         "hostcomplete",
		defaultState: true,
	},
	{name: "huponexit"},
	{
		name:         "inherit_errexit",
		defaultState: true,
	},
	{
		name:         "interactive_comments",
		defaultState: true,
	},
	{name: "lithist"},
	{name: "localvar_inherit"},
	{name: "localvar_unset"},
	{name: "login_shell"},
	{name: "mailwarn"},
	{name: "no_empty_cmd_completion"},
	{name: "nocasematch"},
	{
		name:         "progcomp",
		defaultState: true,
	},
	{name: "progcomp_alias"},
	{
		name:         "promptvars",
		defaultState: true,
	},
	{name: "restricted_shell"},
	{name: "shift_verbose"},
	{
		name:         "sourcepath",
		defaultState: true,
	},
	{name: "xpg_echo"},
}

// To access the shell options arrays without a linear search when we
// know which option we're after at compile time. First come the shell options,
// then the bash options.
const (
	// These correspond to indexes in [shellOptsTable]
	optAllExport = iota
	optErrExit
	optErrTrace
	optFuncTrace
	optNoClobber
	optNoExec
	optNoGlob
	optNoUnset
	optXTrace
	optPipeFail
	optBraceExpand
	optPhysical
	optNotify
	optMonitor
	optHistExpand
	optHistory
	optEmacs
	optVi
	optVerbose
	optPosix
	optHashAll
	optInteractiveComments
	optKeyword
	optOneCmd
	optPrivileged
	optIgnoreEOF
	optNoLog

	// These correspond to indexes (offset by the whole of
	// [posixOptsTable]) of supported options in [bashOptsTable]
	optDotGlob
	optExpandAliases
	optExtDebug
	optExtGlob
	optFailGlob
	optGlobStar
	optLastPipe
	optNoCaseGlob
	optNullGlob
)

// Reset returns a runner to its initial state, right before the first call to
// Run or Reset.
//
// Typically, this function only needs to be called if a runner is reused to run
// multiple programs non-incrementally. Not calling Reset between each run will
// mean that the shell state will be kept, including variables, options, and the
// current directory.
func (r *Runner) Reset() {
	if !r.usedNew {
		panic("use interp.New to construct a Runner")
	}
	if !r.didReset {
		r.origDir = r.Dir
		r.origParams = r.Params
		r.origOpts = r.opts
		r.origStdin = r.stdin
		r.origStdout = r.stdout
		r.origStderr = r.stderr

		if r.execHandler != nil && len(r.execMiddlewares) > 0 {
			panic("interp.ExecHandler should be replaced with interp.ExecHandlers, not mixed")
		}
		if r.execHandler == nil {
			r.execHandler = DefaultExecHandler(2 * time.Second)
		}
		// Middlewares are chained from first to last, and each can call the
		// next in the chain, so we need to construct the chain backwards.
		for _, mw := range slices.Backward(r.execMiddlewares) {
			r.execHandler = mw(r.execHandler)
		}
		// Fill tempDir; only need to do this once given that Env will not change.
		if dir := r.Env.Get("TMPDIR").String(); filepath.IsAbs(dir) {
			r.tempDir = dir
		} else {
			r.tempDir = os.TempDir()
		}
		// Clean it as we will later do a string prefix match.
		r.tempDir = filepath.Clean(r.tempDir)
	}
	// reset the internal state
	*r = Runner{
		Env:            r.Env,
		tempDir:        r.tempDir,
		callHandler:    r.callHandler,
		execHandler:    r.execHandler,
		openHandler:    r.openHandler,
		readDirHandler: r.readDirHandler,
		statHandler:    r.statHandler,
		accessHandler:  r.accessHandler,

		// These can be set by functions like [Dir] or [Params], but
		// builtins can overwrite them; reset the fields to whatever the
		// constructor set up.
		Dir:    r.origDir,
		Params: r.origParams,
		opts:   r.origOpts,
		stdin:  r.origStdin,
		stdout: r.origStdout,
		stderr: r.origStderr,

		origDir:    r.origDir,
		origParams: r.origParams,
		origOpts:   r.origOpts,
		origStdin:  r.origStdin,
		origStdout: r.origStdout,
		origStderr: r.origStderr,

		// emptied below, to reuse the space
		Vars: r.Vars,

		// Constructor state, not run state: which script this runner was
		// started on does not change between runs, and a Reset that
		// forgot it would empty BASH_SOURCE at the top level (#266).
		mainScript: r.mainScript,

		// Constructor state too: the hooks are installed once by the shell
		// and must survive the Reset that Run performs on first use.
		historyHook: r.historyHook,
		traceHook:   r.traceHook,

		dirStack: r.dirStack[:0],
		usedNew:  r.usedNew,
	}
	// Ensure we stop referencing any pointers before we reuse bgProcs.
	clear(r.bgProcs)
	r.bgProcs = r.bgProcs[:0]

	if r.Vars == nil {
		r.Vars = make(map[string]expand.Variable)
	} else {
		clear(r.Vars)
	}
	// TODO(v4): Use the supplied Env directly if it implements enough methods.
	r.writeEnv = &overlayEnviron{parent: r.Env}
	if !r.writeEnv.Get("HOME").IsSet() {
		home, _ := os.UserHomeDir()
		r.setVarString("HOME", home)
	}
	if !r.writeEnv.Get("UID").IsSet() {
		r.setVar("UID", expand.Variable{
			Set:      true,
			Kind:     expand.String,
			ReadOnly: true,
			Str:      strconv.Itoa(os.Getuid()),
		})
	}
	if !r.writeEnv.Get("EUID").IsSet() {
		r.setVar("EUID", expand.Variable{
			Set:      true,
			Kind:     expand.String,
			ReadOnly: true,
			Str:      strconv.Itoa(os.Geteuid()),
		})
	}
	if !r.writeEnv.Get("GID").IsSet() {
		r.setVar("GID", expand.Variable{
			Set:      true,
			Kind:     expand.String,
			ReadOnly: true,
			Str:      strconv.Itoa(os.Getgid()),
		})
	}
	r.setVarString("PWD", r.Dir)
	r.setVarString("IFS", " \t\n")
	r.setVarString("OPTIND", "1")

	r.dirStack = append(r.dirStack, r.Dir)

	r.didReset = true
}

// ExitStatus is a non-zero status code resulting from running a shell node.
type ExitStatus uint8

func (s ExitStatus) Error() string { return fmt.Sprintf("exit status %d", s) }

// NewExitStatus creates an error which contains the specified exit status code.
//
// Deprecated: use [ExitStatus] directly.
//
//go:fix inline
func NewExitStatus(status uint8) error {
	return ExitStatus(status)
}

// IsExitStatus checks whether error contains an exit status and returns it.
//
// Deprecated: use [errors.As] with [ExitStatus] directly.
//
//go:fix inline
func IsExitStatus(err error) (status uint8, ok bool) {
	var es ExitStatus
	if errors.As(err, &es) {
		return uint8(es), true
	}
	return 0, false
}

// Run interprets a node, which can be a [*File], [*Stmt], or [Command]. If a non-nil
// error is returned, it will typically contain a command's exit status, which
// can be retrieved with [IsExitStatus].
//
// Run can be called multiple times synchronously to interpret programs
// incrementally. To reuse a [Runner] without keeping the internal shell state,
// call Reset.
//
// Calling Run on an entire [*File] implies an exit, meaning that an exit trap may
// run.
func (r *Runner) Run(ctx context.Context, node syntax.Node) error {
	if !r.didReset {
		r.Reset()
	}
	r.fillExpandConfig(ctx)
	r.exit = exitStatus{}
	// A runner is Run more than once — an rc file, then the session — and
	// each run's exit is its own: a mark left by the last run would
	// silence this one's EXIT trap entirely.
	r.exitTrapFired = false
	r.filename = ""
	switch node := node.(type) {
	case *syntax.File:
		r.filename = node.Name
		r.stmtsTopLevel(ctx, node.Stmts)
	case *syntax.Stmt:
		r.stmt(ctx, node)
	case syntax.Command:
		r.cmd(ctx, node)
	default:
		return fmt.Errorf("node can only be File, Stmt, or Command: %T", node)
	}
	// A bare Command bypasses stmt, which normally updates lastExit.
	r.lastExit = r.exit
	// Running an entire file implies an exit; a statement or command
	// only exits the shell via the exit builtin, errexit, and so on.
	if _, ok := node.(*syntax.File); ok || r.exit.exiting {
		// A signal that arrived during the last command has had no
		// statement boundary to fire at, so give it one — dropping a
		// trap because its signal came in the script's final moment
		// would make `kill -X $$` on the last line a silent no-op.
		r.runPendingSignalTraps(ctx)
		if !r.exitTrapFired {
			r.exitTrapFired = true
			// The keep-flow path, not the restoring one: an `exit` inside
			// an EXIT trap replaces the status — `trap "exit 9" EXIT;
			// exit 3` answers 9 — while an ordinary failing command in
			// the action still restores it, since the body's first
			// statement clears the in-flight exiting flag (#353).
			r.runTrapCallback(ctx, r.callbackExit, "exit", r.callbackExitLine, true)
		}
	}
	maps.Insert(r.Vars, r.writeEnv.Each)
	// Return the first of: a fatal error, a non-fatal handler error, or the exit code.
	if err := r.exit.err; err != nil {
		if r.exit.code == 0 {
			// This should never happen; too much code relies on checking [exitStatus.code]
			// to see if the last command succeeded or failed. [exitStatus.err] should only be
			// additional information, so fail loudly if the invariant is broken.
			panic("ended up with a non-nil exitStatus.err but a zero exitStatus.code")
		}
		return err
	}
	if code := r.exit.code; code != 0 {
		return ExitStatus(code)
	}
	return nil
}

// Exited reports whether the last Run call should exit an entire shell. This
// can be triggered by the "exit" built-in command, for example.
//
// Note that this state is overwritten at every Run call, so it should be
// checked immediately after each Run call.
// Environ returns the runner's variables as they are now, including those a
// script has assigned while running.
//
// Note that [Runner.Vars] and [Runner.Env] describe the state the runner was
// built with; they are not updated as it runs, so enumerating them yields the
// environment the shell started from rather than its current one. That
// distinction is easy to miss, as the names they do return are all real.
func (r *Runner) Environ() expand.Environ {
	return r.writeEnv
}

func (r *Runner) Exited() bool {
	return r.exit.exiting
}

// Subshell makes a copy of the given [Runner], suitable for use concurrently
// with the original. The copy will have the same environment, including
// variables and functions, but they can all be modified without affecting the
// original.
//
// Subshell is not safe to use concurrently with [Run]. Orchestrating this is
// left up to the caller; no locking is performed.
//
// To replace e.g. stdin/out/err, do [StdIO](r.stdin, r.stdout, r.stderr)(r) on
// the copy.
func (r *Runner) Subshell() *Runner {
	return r.subshell(true)
}

// subshell is like [Runner.subshell], but allows skipping some allocations and copies
// when creating subshells which will not be used concurrently with the parent shell.
// TODO(v4): we should expose this, e.g. SubshellForeground and SubshellBackground.
func (r *Runner) subshell(background bool) *Runner {
	if !r.didReset {
		r.Reset()
	}
	// Keep in sync with the Runner type. Manually copy fields, to not copy
	// sensitive ones like [errgroup.Group], and to do deep copies of slices.
	r2 := &Runner{
		Dir:            r.Dir,
		tempDir:        r.tempDir,
		Params:         r.Params,
		callHandler:    r.callHandler,
		execHandler:    r.execHandler,
		openHandler:    r.openHandler,
		readDirHandler: r.readDirHandler,
		statHandler:    r.statHandler,
		accessHandler:  r.accessHandler,
		stdin:          r.stdin,
		stdout:         r.stdout,
		stderr:         r.stderr,
		bgWriteMu:      r.bgWriteMu,
		filename:       r.filename,
		opts:           r.opts,
		usedNew:        r.usedNew,
		exit:           r.exit,
		lastExit:       r.lastExit,

		origStdout: r.origStdout, // used for process substitutions

		// A subshell is its own level for the ERR trap, and it only inherits
		// the trap at all when "errtrace" is set -- which is why callbackErr is
		// copied here rather than unconditionally.
		errTrapDepth: r.errTrapDepth + 1,
	}
	if r.opts[optErrTrace] {
		r2.callbackErr = r.callbackErr
	}
	// DEBUG is inherited by a subshell only under "functrace", which is the
	// rule bash applies to it and to RETURN. Measured rather than assumed:
	// `trap 'echo D' DEBUG; ( echo sub )` prints nothing from the subshell.
	if r.opts[optFuncTrace] {
		r2.callbackDebug = r.callbackDebug
	}
	// The trace hook crosses unconditionally: it is the shell's own
	// instrumentation, not a script's trap, so no shell option governs
	// whether a subshell or pipeline stage is traced (#474).
	r2.traceHook = r.traceHook
	// A subshell is the same frame as far as RETURN is concerned: it is
	// not a function call, so nothing about it changes reachability.
	r2.callbackReturn = r.callbackReturn
	r2.returnTrapOff = r.returnTrapOff
	r2.listed = r.listed
	r2.sigListed = maps.Clone(r.sigListed)
	// The frame stack crosses into a subshell, because `$(caller 0)` and
	// `$(trap -p)` are how a script asks these questions at all — a
	// command substitution that reported an empty stack would answer
	// nothing to the only spelling anyone uses.
	r2.frames = slices.Clone(r.frames)
	r2.mainScript = r.mainScript
	r2.funcSource = maps.Clone(r.funcSource)
	r2.extraFiles = maps.Clone(r.extraFiles)
	r2.writeEnv = newOverlayEnviron(r.writeEnv, background)
	// Funcs are copied, since they might be modified.
	r2.Funcs = maps.Clone(r.Funcs)
	r2.Vars = make(map[string]expand.Variable)
	r2.alias = maps.Clone(r.alias)

	r2.dirStack = append(r2.dirBootstrap[:0], r.dirStack...)
	r2.fillExpandConfig(r.ectx)
	r2.didReset = true
	return r2
}
