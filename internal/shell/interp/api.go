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
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/syntax"
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

	// exportedFuncs holds the names `export -f` marked, which travel to
	// children as BASH_FUNC_<name>%% environment entries (#387).
	exportedFuncs map[string]bool

	// readonlyFuncs holds the names `readonly -f` and `declare -fr`
	// marked (#615). Unlike exportedFuncs this never leaves the shell:
	// measured, a child inheriting an exported function may redefine it
	// freely, so the bit is per-shell state rather than part of the
	// function's environment representation.
	readonlyFuncs map[string]bool

	// tracedFuncs holds the names `declare -ft` marked. The trace
	// attribute is what lets the DEBUG and RETURN traps reach *into* one
	// function without `set -T` turning them on for every function
	// (#697), so it is read on entry as well as printed in a listing —
	// an attribute that only showed up in `declare -pF` would be a flag
	// a debugger sets and nothing acts on.
	tracedFuncs map[string]bool

	// localOpts holds the shell options `local -` saved in the running
	// function, to be put back when it returns (#385).
	localOpts *runnerOpts

	// declTempBound holds the names that temp-env prefix carries, so a
	// declaration can tell that the value it is looking at is the
	// binding rather than an outer variable — a new local takes the
	// temp value instead of starting unset (#380 with #381).
	declTempBound map[string]bool

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
	dirStack []string
	// testCallName is the name the running test was invoked by --
	// `test`, `[` or `[[` -- which is how bash addresses its operand
	// diagnostics (#401).
	testCallName string

	// startTime and secondsBase back SECONDS, which counts from the
	// shell's start unless a script assigns it (#408).
	startTime   time.Time
	secondsBase int
	// bashPIDValue distinguishes subshells for BASHPID: koi runs a
	// subshell in the same process, so the number is per-context
	// rather than a real pid.
	bashPIDValue int
	// argv0 is BASH_ARGV0's writable view of $0.
	argv0 string
	// varHooks are the callbacks [VarHook] installed, keyed by variable
	// name. Constructor state, like the other hooks.
	varHooks map[string]func(string, string)
	// random backs $RANDOM: per-runner, so a subshell's draws do not
	// advance this one's, and replaced outright when a script assigns
	// RANDOM to seed it (#547).
	random *mathrand.Rand
	// readDynamic records the computed variables something has asked for
	// a value from, which is what decides whether a *listing* shows one
	// with its value or with none (#689). bash caches a dynamic
	// variable's value on first use and its listings print the cache, so
	// `declare -a` before anything reads DIRSTACK is `declare -a
	// DIRSTACK=()` and the same command afterwards carries the entries.
	// See [lazyListing] for which names, and why it is per name.
	readDynamic map[string]bool

	// wroteDynamic is readDynamic's other half: a write fills the same
	// cache a read does, so a listing after `SECONDS=10` carries a value.
	// They are two records rather than one because they answer the same
	// question about the *value* and different questions about the
	// integer attribute — see [Runner.dynamicListingAttrs].
	wroteDynamic map[string]bool

	// unsetDynamic records the computed variables a script has unset,
	// which ends their specialness for the rest of the shell.
	unsetDynamic map[string]bool
	// traceLine is the line of the statement being run, which PS4's
	// $LINENO reports (#413) and which locates a diagnostic (#571).
	traceLine uint
	// lineOffset numbers a separately parsed chunk — an eval'd string, a
	// trap action — as if it were spliced in where it was written. See
	// [Runner.shiftLines].
	lineOffset uint
	// preRedirStderr is where an expansion error is reported: the
	// stderr in force before the current statement's own redirections
	// (#469).
	preRedirStderr io.Writer
	// expandingAlias guards against an alias that names itself, which
	// expands once and then means the command (#407).
	expandingAlias map[string]bool
	// disabledBuiltins are the names `enable -n` turned off, which fall
	// through to PATH like any other command (#411).
	disabledBuiltins map[string]bool
	// hashTable is `hash -p`'s name-to-path map, consulted before a
	// PATH search.
	hashTable    map[string]string
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
	// sigIgnoredAtEntry are the signals the shell was started with
	// ignored (#441). POSIX says a non-interactive shell may neither
	// trap nor reset one, and bash lists them — koi never looked, so
	// its listing omitted them and `trap 'cmd' SIG` armed a handler
	// bash refuses to arm.
	sigIgnoredAtEntry map[string]bool
	sigChan           chan os.Signal
	sigNames          map[os.Signal]string

	// Where each non-command trap was set, for $LINENO inside its action
	// (#352): bash counts an EXIT or signal trap's action lines from the
	// line of the `trap` command that installed it, where DEBUG and ERR
	// count from the line of the command that triggered them. RETURN
	// looked like it belonged in the first group and does not — it
	// counts from the returning frame's own line, so its number comes
	// from the call site rather than from here (#614).
	callbackExitLine uint
	sigTrapLines     map[string]uint

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

	// debugTrapOff is returnTrapOff's twin for DEBUG, which bash governs
	// with the same switch. It became a flag rather than a question asked
	// at the firing point when the trace attribute arrived (#697): with
	// only `set -T` to consult, "am I in a function?" and "were the traps
	// off when I entered one?" are the same answer, and with a per
	// function attribute they are not — a traced function called from an
	// untraced one inherits nothing, so the state has to be remembered
	// from the entry that decided it.
	debugTrapOff bool

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

	// origExtraFiles is what [InheritedFiles] was given: the descriptors
	// the process was started with, which a Reset has to put back.
	origExtraFiles map[int]io.ReadWriteCloser

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

	// varRedirFds are the descriptors a {varname} redirection allocated
	// in the statement being run. They outlive it, unlike every other
	// redirection, so the restore keeps them rather than dropping them
	// with the rest of the table (#418).
	varRedirFds []int

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

	// disowned marks a job `disown` forgot: it is no longer listed and
	// no longer waited for (#397).
	disowned bool

	// cancel ends the job, and killed carries the signal number a `kill
	// %n` asked for so the job can report 128+n the way a signaled
	// process does (#397). koi's jobs are goroutines, so the signal is
	// delivered as a cancelled context — which is how the shell already
	// interrupts a running command — and reaches a real child through
	// the exec handler's own context.
	cancel context.CancelFunc
	killed *atomic.Int32

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

// alias holds what bash holds: the replacement *text*, spliced into
// the command line and re-parsed at expansion time (#407). koi parsed
// the value when the alias was defined, as a standalone command, so
// most real aliases were refused outright — `alias ok='echo OK >&2'`
// is not a word list, and neither is anything with a `;` or a
// newline in it.
type alias struct {
	text  string
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
	for name, als := range r.alias {
		out[name] = als.text
	}
	return out
}

// DirStack returns a copy of the pushd/popd stack, top (the current
// directory) first — the order `dirs` prints, which is the order the
// stack is now stored in (#390).
func (r *Runner) DirStack() []string {
	r.dirStackSync()
	return slices.Clone(r.dirStack)
}

// ParserOptions are the parser options this runner's shell options
// currently call for. A shell reading a script line by line applies them
// to its parser between lines, so an option a script sets reaches the
// rest of the script (#450).
func (r *Runner) ParserOptions() []syntax.ParserOption {
	return []syntax.ParserOption{
		syntax.POSIXMode(r.opts[optPosix]),
		// `shopt -s extglob` changes how the rest of the script is
		// *parsed*, not how it globs: with it off, `echo +(a|b)c` is a
		// syntax error at the `(` rather than a pattern that declines to
		// match (#619).
		syntax.ExtendedGlobs(r.opts[optExtGlob]),
	}
}

// Stdin is the file the runner's descriptor 0 currently refers to.
//
// It changes when a script redirects the *shell's* own fd 0, as
// `exec 0< file` does. A shell reading its commands from standard input
// watches this: redirecting fd 0 there switches the command stream, so
// the rest of the script comes from the new file (#516). It is nil when
// the runner was built without standard input.
func (r *Runner) Stdin() *os.File { return r.stdin }

// OptionSet reports whether the named `set -o` or `shopt` option is on
// right now. An unknown or unsupported name reports false, which is what
// an option that can never leave its default amounts to.
//
// Unlike [Runner.Options] this reads one name without building the whole
// list, because a shell asks between input lines — `set -o posix`
// changes how the rest of a script is *parsed* (#450), so the question
// is asked once per line.
func (r *Runner) OptionSet(name string) bool {
	for i, opt := range posixOptsTable {
		if opt.name == name {
			return opt.supported && r.opts[i]
		}
	}
	for i, opt := range bashOptsTable {
		if opt.name == name {
			return opt.settable() && r.opts[len(posixOptsTable)+i]
		}
	}
	return false
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
		if opt.settable() {
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

// VarHook installs a callback for assignments to the named variables,
// which is how the shell around the interpreter learns about a variable
// whose *assignment* is an action rather than a value.
//
// HISTFILESIZE is the case it exists for (#491): assigning it truncates
// $HISTFILE on the spot, and the history file belongs to the shell
// rather than to the interpreter. The callback runs after the variable
// is set, so a hook that reads it sees the new value.
func VarHook(names []string, fn func(name, value string)) RunnerOption {
	return func(r *Runner) error {
		if fn == nil || len(names) == 0 {
			return nil
		}
		if r.varHooks == nil {
			r.varHooks = make(map[string]func(string, string), len(names))
		}
		for _, n := range names {
			r.varHooks[n] = fn
		}
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
// setUsageError is an invalid option *letter*, which bash follows with
// set's usage line. An invalid `-o` *name* is not one of these: bash
// prints that message on its own, measured.
type setUsageError struct{ msg string }

func (e setUsageError) Error() string { return e.msg }

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
					return setUsageError{fmt.Sprintf("%s: invalid option", flag)}
				}
				if err := r.setPosixOpt(status, opt, enable); err != nil {
					return err
				}
				continue
			}
			// `-o` takes an option *name*, and only as a separate word
			// that does not itself begin with a sign: `set -o -B` is the
			// listing followed by braceexpand and `set -oe` is the
			// listing followed by errexit, both measured. koi read the
			// next word whatever it was, so bash's own builtins.tests
			// counted the listing at one line instead of twenty-seven.
			value := ""
			if fp.current == "" && len(fp.remaining) > 0 {
				if a := fp.remaining[0]; a == "" || (a[0] != '-' && a[0] != '+') {
					value = fp.value()
				}
			}
			if value == "" && enable {
				for _, i := range posixOptNames() {
					r.printOptLine(posixOptsTable[i].name, setOptColumn, r.opts[i])
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
				return fmt.Errorf("%s: invalid option name", value)
			}
			if err := r.setPosixOpt(status, opt, enable); err != nil {
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
	if dup, ok := r.(shellFD); ok {
		// `exec 0< /dev/stdin` and friends name a descriptor the shell
		// already holds (#645), so the file behind it is the answer — a
		// copying goroutine would consume input nothing asked it to read.
		if f := dup.file(); f != nil {
			return f, nil
		}
	}
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
// IgnoredSignals records the signals that were ignored when the process
// started, which the shell around the interpreter has to find out (#441)
// — reading a disposition needs sigaction, and os/signal has no query.
//
// A signal named here is listed by `trap` with an empty action, and a
// script's attempt to trap or reset it is refused, as POSIX requires of
// a non-interactive shell.
func IgnoredSignals(names []string) RunnerOption {
	return func(r *Runner) error {
		if len(names) == 0 {
			return nil
		}
		r.sigIgnoredAtEntry = make(map[string]bool, len(names))
		for _, n := range names {
			name, _, ok := lookupSignal(n)
			if !ok {
				continue
			}
			r.sigIgnoredAtEntry[name] = true
		}
		return nil
	}
}

// InheritedFiles seeds the descriptors above 2 with files the process
// already had open — what a shell inherits from whoever started it, so
// that `koi 3<&0 script` can do `exec <&3` (#419).
//
// This is the shell's business rather than the interpreter's, which is
// why it is an option and not a scan done here: a program embedding a
// runner has descriptors of its own open and handing them to a script
// would be a surprise, not a feature.
//
// The files are the caller's; the runner never closes them.
func InheritedFiles(files map[int]*os.File) RunnerOption {
	return func(r *Runner) error {
		if len(files) == 0 {
			return nil
		}
		r.origExtraFiles = make(map[int]io.ReadWriteCloser, len(files))
		for fd, f := range files {
			if fd <= 2 || f == nil {
				// 0, 1 and 2 are StdIO's, and a descriptor which is not
				// there is not a descriptor.
				continue
			}
			r.origExtraFiles[fd] = f
		}
		r.extraFiles = maps.Clone(r.origExtraFiles)
		return nil
	}
}

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
		if opt.name == name && !letterOnlyOpts[name] {
			return &r.opts[i], opt
		}
	}
	return nil, posixOpt{}
}

// IsSetOptionFlag reports whether c is a `set` single-letter option that
// this interpreter knows — supported or not.
//
// The shell around it needs this to tell a set letter in argv from a
// typo: bash accepts any of them there (`bash -euxc …`) and answers a
// usage error with status 2 for anything else (#426). Knowing the letter
// is the whole question here; whether koi can honor it is the
// interpreter's business, and it says so itself when asked.
func IsSetOptionFlag(c byte) bool {
	if c == ' ' {
		return false // the table's "no flag form" marker
	}
	for _, opt := range &posixOptsTable {
		if opt.flag == c {
			return true
		}
	}
	return false
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
func (r *Runner) setPosixOpt(status *bool, opt posixOpt, enable bool) error {
	if opt.name == "restricted" && *status && !enable {
		// A restricted shell cannot un-restrict itself, which is the
		// one rule the whole feature rests on (#398). bash reports it
		// as an invalid option, since `+r` is not a spelling it takes.
		return fmt.Errorf("+r: invalid option")
	}
	if opt.supported || enable == *status {
		*status = enable
		r.excludeEditMode(opt.name, enable)
		return nil
	}
	state := "off"
	if enable {
		state = "on"
	}
	return fmt.Errorf("cannot turn %s %s: not implemented", opt.name, state)
}

// excludeEditMode keeps `emacs` and `vi` from being on at once, which is
// the one rule in the `set -o` table that couples two options (#576).
//
// It is one-directional, and that is measured rather than symmetric-by
// -assumption: turning either one *on* turns the other off, while turning
// one off leaves the other exactly as it was — `set -o vi; set +o vi`
// ends with both off in bash rather than back in emacs mode, and a shell
// with neither bit set is still editing in emacs, since that is what
// readline does with no mode asked for.
func (r *Runner) excludeEditMode(name string, enable bool) {
	if !enable {
		return
	}
	switch name {
	case "emacs":
		r.opts[optVi] = false
	case "vi":
		r.opts[optEmacs] = false
	}
}

// shellOptsList and bashOptsList render SHELLOPTS and BASHOPTS: the
// names that are on, colon-separated, in the order each listing prints
// them. bash spells the set -o names with hyphens
// (interactive-comments), which is the spelling `set -o` itself uses.
func (r *Runner) shellOptsList() string {
	var on []string
	for _, i := range posixOptNames() {
		if r.opts[i] {
			on = append(on, posixOptsTable[i].name)
		}
	}
	return strings.Join(on, ":")
}

func (r *Runner) bashOptsList() string {
	var on []string
	for _, i := range bashOptNames() {
		if r.opts[len(posixOptsTable)+i] {
			on = append(on, bashOptsTable[i].name)
		}
	}
	return strings.Join(on, ":")
}

// bashOptNames lists the shopt options by name, which is how bash
// prints them; the table itself groups the supported ones first.
func bashOptNames() []int {
	indexes := make([]int, len(bashOptsTable))
	for i := range indexes {
		indexes[i] = i
	}
	slices.SortFunc(indexes, func(a, b int) int {
		return strings.Compare(bashOptsTable[a].name, bashOptsTable[b].name)
	})
	return indexes
}

// OptionNames lists every `set -o` name the interpreter recognizes, in
// the order bash lists them — by name.
//
// Every name, not only the ones koi can change: what a completion is
// answering is which options this shell *knows*, and refusing to list
// one koi keeps at its default would offer a shorter menu than the
// shell itself accepts. [Runner.Options] is the other question — what
// each one is set to right now — and it is right for that one to skip
// them.
func OptionNames() []string {
	out := make([]string, 0, len(posixOptsTable))
	for _, i := range posixOptNames() {
		out = append(out, posixOptsTable[i].name)
	}
	return out
}

// ShoptNames is [OptionNames] for the `shopt` table.
func ShoptNames() []string {
	out := make([]string, 0, len(bashOptsTable))
	for _, i := range bashOptNames() {
		out = append(out, bashOptsTable[i].name)
	}
	return out
}

// TrapNames lists every spec `trap` accepts, in the order bash's
// `compgen -A signal` lists them: EXIT first, the real signals in signal
// -number order with readline's SIG prefix, then the three fake traps
// (#606).
//
// This shell's set, not bash's, on the rule [OptionNames] states: koi's
// table is the portable one internal/jobs states the case for, so it is
// shorter than bash's on any given platform, and a completion offering a
// name `trap` would then refuse is worse than one that is short. The
// prefix is bash's spelling rather than the table's — `trap -l` prints
// `SIGHUP` too — and `trap` takes it either way.
func TrapNames() []string {
	sigs := signalList()
	out := make([]string, 0, len(sigs)+4)
	out = append(out, "EXIT")
	for _, s := range sigs {
		out = append(out, "SIG"+s.name)
	}
	return append(out, "DEBUG", "ERR", "RETURN")
}

// JobEntry is one row of the shell's job table, for the consumers that
// are not the `jobs` builtin: the command as it was written and whether
// it is still running (#606).
type JobEntry struct {
	Command string
	Running bool
}

// JobEntries reports the jobs this shell would list, newest first.
//
// Newest first because that is the order bash's own `compgen -A job`
// answers in — measured, and the opposite of the `jobs` listing's — and
// a completion offering the most recent job first is the one a `fg`
// wants. The visibility rules are the `jobs` builtin's: a disowned job
// is gone (#397), and a finished one that has already been reported is
// off the table. Nothing here marks a job as reported, because asking
// what the table holds must not change what the next `jobs` prints.
func (r *Runner) JobEntries() []JobEntry {
	out := make([]JobEntry, 0, len(r.bgProcs))
	for i := len(r.bgProcs) - 1; i >= 0; i-- {
		job := &r.bgProcs[i]
		running := !job.finished()
		if job.disowned || (!running && job.reported) {
			continue
		}
		out = append(out, JobEntry{Command: job.cmd, Running: running})
	}
	return out
}

// posixOptNames lists the options the way bash prints them, which is by
// name rather than in the order the table happens to be in.
func posixOptNames() []int {
	idx := make([]int, len(posixOptsTable))
	for i := range idx {
		idx[i] = i
	}
	idx = slices.DeleteFunc(idx, func(i int) bool { return letterOnlyOpts[posixOptsTable[i].name] })
	slices.SortFunc(idx, func(a, b int) int {
		return strings.Compare(posixOptsTable[a].name, posixOptsTable[b].name)
	})
	return idx
}

func (r *Runner) bashOptByName(name string) (status *bool, supported bool) {
	for i, opt := range bashOptsTable {
		if opt.name == name {
			index := len(posixOptsTable) + i
			return &r.opts[index], opt.settable()
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

// letterOnlyOpts have no `-o name` spelling: `restricted` is reachable
// as -r and is neither listed by `set -o` nor accepted as `set -o
// restricted` (#398). Measured — the differential listing test caught
// it being listed.
//
// It is a set rather than a field on posixOpt because the table is
// written positionally, and one exception is not worth rewriting
// twenty-eight rows for.
var letterOnlyOpts = map[string]bool{"restricted": true}

type bashOpt struct {
	name         string
	defaultState bool       // Bash's default value for this option
	support      optSupport // what koi can do about being asked to move it
}

// optSupport says what happens when something asks a shopt to leave its
// default, and there are three answers rather than two (#575).
//
// The third one is the reason this is not a bool. A handful of bash's
// options govern behavior that is not the *interpreter's* — cd spelling
// correction at a prompt, what happens after a history expansion, whether
// programmable completion runs — and koi's answers to all of those come
// from its own line editor and completion, not from a bash bit. For those
// the bit is real state and recording it is not a pretence: nothing a
// script can observe is being faked, and refusing instead costs the
// script its state for nothing. Everything that would change what a
// script observes keeps the honest refusal, because a shell that says it
// is in a mode it is not is worse than one that says no.
type optSupport uint8

const (
	// optUnimplemented: koi cannot put this option in its other state,
	// and says so rather than accepting the request (#542).
	optUnimplemented optSupport = iota
	// optImplemented: the interpreter acts on this option.
	optImplemented
	// optStateOnly: the bit is tracked and answered; the behavior it
	// names belongs to the shell around the interpreter.
	optStateOnly
)

// settable reports whether this option can leave its default at all —
// either because koi implements it or because its bit is state koi
// records. It is what `shopt -s` is allowed to move and what the option
// listings for a *session* report.
func (o bashOpt) settable() bool { return o.support != optUnimplemented }

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
	// notify and monitor are job control (#5). verbose would have to
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
	// Job control, which a script asks for with `set -m` (#397). koi
	// refused it, so the line was fatal — and the scripts that write it
	// are the ones that most want the shell to keep going.
	//
	// What it means here is narrower than in bash and is worth stating,
	// because the difference is invisible until it matters: koi's
	// background jobs are goroutines rather than process groups of their
	// own, so `-m` does not change how a job is placed or signaled. What
	// it does change is what a script can *say*: `fg` and `bg` are gated
	// on job control being on, exactly as in bash, and `lastpipe` stops
	// applying, which is bash's rule too and is observable.
	{'m', "monitor", false, true},
	// History expansion (#559). It is off in a non-interactive shell and
	// a script turns it on with `set -H`, which koi refused — so `!!` in
	// a script was an ordinary command name and every later line of
	// bash's own histexp.tests diverged. The expansion itself is a
	// transformation of the *line*, applied before parsing, so it lives
	// in the shell around the interpreter; what lives here is the bit,
	// because the bit is what a line changes and what the next line is
	// read under. See [ScriptReader.Filter] for the seam and #96 for the
	// expander.
	//
	// The gate is *both* this and history, measured in both directions:
	// `set -H` alone expands nothing, and `set +o history` after both
	// were on stops the expansion rather than merely stopping the
	// recording.
	{'H', "histexpand", false, true},
	{' ', "history", false, true},
	// The line editor's dialect (#576). The behavior is the shell's
	// around the interpreter rather than the interpreter's own, which is
	// why these two spent so long refusing to move — and refusing was the
	// wrong answer for exactly the reason #575 gives for `cdspell`: the
	// bit is real state, nothing a script can observe is being faked by
	// recording it, and koi *does* act on the request, so not recording it
	// left the option disagreeing with the shell's own behavior. See
	// [Runner.excludeEditMode] for the one rule that is not an ordinary
	// option's: they are mutually exclusive.
	{' ', "emacs", false, true},
	{' ', "vi", false, true},
	{'v', "verbose", false, false},
	// POSIX mode (#395). koi refused it, which was honest and cost
	// whole suite files: a script that opens with `set -o posix` got
	// exit 2 and every later assertion diverged. What it changes here
	// is what a script can observe — see the semantics keyed on
	// optPosix — rather than every difference bash lists.
	{' ', "posix", false, true},
	{'h', "hashall", true, false},
	{' ', "interactive-comments", true, false},
	// keyword is implemented (#396): every assignment-shaped word goes
	// into the command's environment, not just the leading ones.
	{'k', "keyword", false, true},
	{'t', "onecmd", false, false},
	{'p', "privileged", false, false},
	// A restricted shell (#398). It is a *compatibility* feature and
	// not a security boundary — bash's own manual says a restricted
	// shell can be escaped through any program that runs a subshell,
	// and koi's answer to confinement is the sandbox profiles on the
	// exec path. What it buys here is that rsh.tests' probes behave,
	// and that a script asking for it is not refused outright.
	{'r', "restricted", false, true},
	// ignoreeof governs what an *interactive* shell does with EOF, so
	// there is nothing for this runner to do — but refusing it made
	// `set -o ignoreeof` in an rc abort rather than be irrelevant
	// (#396). The shell around the interpreter owns the behavior.
	{' ', "ignoreeof", false, true},
	{' ', "nolog", false, false},
}

var bashOptsTable = [...]bashOpt{
	// supported options, sorted alphabetically by name
	{
		name:         "dotglob",
		defaultState: false,
		support:      optImplemented,
	},
	{
		name:         "expand_aliases",
		defaultState: false,
		support:      optImplemented,
	},
	{
		name:         "extdebug",
		defaultState: false,
		support:      optImplemented,
	},
	{
		name:         "extglob",
		defaultState: false,
		support:      optImplemented,
	},
	{
		name:         "failglob",
		defaultState: false,
		support:      optImplemented,
	},
	{
		name:         "globstar",
		defaultState: false,
		support:      optImplemented,
	},
	{
		// A command substitution runs without errexit unless this asks
		// for it (#412), so the option lands with the fix it governs.
		name:         "inherit_errexit",
		defaultState: false,
		support:      optImplemented,
	},
	{
		// Off by default, as in bash — and until #277 it was effectively
		// always on: the last pipeline stage ran in the current shell, so
		// `cmd | read x` kept x and `cat f | while read l; do n=$((n+1));
		// done` kept n, which is the single most famous bash gotcha
		// answered un-bash-ly. bash additionally requires job control to
		// be inactive for lastpipe to take effect, and so does koi since
		// `set -m` became settable (#397).
		name:         "lastpipe",
		defaultState: false,
		support:      optImplemented,
	},
	{
		// A new local starts unset rather than inheriting the outer
		// variable's value (#381); this option is how bash asks for
		// the inheritance, so it lands with the fix it governs.
		name:         "localvar_inherit",
		defaultState: false,
		support:      optImplemented,
	},
	{
		name:         "nocaseglob",
		defaultState: false,
		support:      optImplemented,
	},
	{
		name:         "nullglob",
		defaultState: false,
		support:      optImplemented,
	},
	{
		// Off by default, as in bash: a {varname} redirection's
		// descriptor outlives the command that opened it, and this is
		// what asks for the other behavior (#418). Appended to the
		// supported block rather than inserted, because the opt*
		// constants below index this table positionally.
		name:         "varredir_close",
		defaultState: false,
		support:      optImplemented,
	},
	{
		// On by default, as in bash 5.2 and later: an unquoted `&` in a
		// replacement is the text that matched (#643). Appended to the
		// supported block rather than inserted, because the opt*
		// constants below index this table positionally.
		name:         "patsub_replacement",
		defaultState: true,
		support:      optImplemented,
	},
	{
		// Escape interpretation is what this option decides, so the bit
		// alone would have been a claim about `echo` that was not true —
		// which is why #542 and #575 both singled it out as a refusal
		// rather than state. The behavior is implemented now, so the
		// refusal is no longer honest (#604). Appended to the supported
		// block for the same reason varredir_close was: the opt*
		// constants index this table positionally.
		name:    "xpg_echo",
		support: optImplemented,
	},
	{
		// Whether `.` searches $PATH for a bare name. koi searched it
		// unconditionally and the shell around the interpreter accepted
		// `shopt -u sourcepath` and dropped it, so turning the search
		// off left the search on — the same accept-and-ignore shape
		// #566 fixed one layer up. Appended for the positional reason
		// above.
		name:         "sourcepath",
		defaultState: true,
		support:      optImplemented,
	},
	{
		// Verify a hashed path before running it, and search again when
		// the program has moved or gone — the option exists because a
		// long-lived shell's hash table goes stale. Appended for the
		// positional reason above.
		name:    "checkhash",
		support: optImplemented,
	},
	// unsupported options, sorted alphabetically by name
	{name: "array_expand_once"},
	{name: "assoc_expand_once"},
	{name: "autocd", support: optStateOnly},
	{name: "bash_source_fullpath"},
	{name: "cdable_vars"},
	{name: "cdspell", support: optStateOnly},
	{name: "checkjobs", support: optStateOnly},
	{
		name:         "checkwinsize",
		defaultState: true,
		support:      optStateOnly,
	},
	{
		name:         "cmdhist",
		defaultState: true,
		support:      optStateOnly,
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
		support:      optStateOnly,
	},
	{name: "direxpand", support: optStateOnly},
	{name: "dirspell", support: optStateOnly},
	{name: "execfail"},
	{
		name:         "extquote",
		defaultState: true,
	},
	{
		name:         "force_fignore",
		defaultState: true,
		support:      optStateOnly,
	},
	{
		// On by default in bash 5.x, which koi had as off — a default
		// is as visible to a script as a setting (#393).
		name:         "globasciiranges",
		defaultState: true,
	},
	{
		name:         "globskipdots",
		defaultState: true,
	},
	{name: "gnu_errfmt"},
	{name: "histappend", support: optStateOnly},
	{name: "histreedit", support: optStateOnly},
	{name: "histverify", support: optStateOnly},
	{
		name:         "hostcomplete",
		defaultState: true,
		support:      optStateOnly,
	},
	{name: "huponexit", support: optStateOnly},
	{
		name:         "interactive_comments",
		defaultState: true,
	},
	{name: "lithist", support: optStateOnly},
	{name: "localvar_unset"},
	{name: "login_shell"},
	{name: "mailwarn", support: optStateOnly},
	{name: "no_empty_cmd_completion", support: optStateOnly},
	{name: "nocasematch"},
	{name: "noexpand_translation"},
	{
		name:         "progcomp",
		defaultState: true,
		support:      optStateOnly,
	},
	{name: "progcomp_alias", support: optStateOnly},
	{
		name:         "promptvars",
		defaultState: true,
		support:      optStateOnly,
	},
	{name: "restricted_shell"},
	{name: "shift_verbose"},
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
	optRestricted
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
	optInheritErrExit
	optLastPipe
	optLocalVarInherit
	optNoCaseGlob
	optNullGlob
	optVarRedirClose
	optPatSubReplacement
	optXpgEcho
	optSourcePath
	optCheckHash
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
		varHooks:    r.varHooks,

		// What the process inherited is constructor state as well: a
		// Reset that forgot it would let a script trap a signal the
		// shell was told to ignore (#441), or stop seeing a descriptor
		// it was started with (#419).
		sigIgnoredAtEntry: r.sigIgnoredAtEntry,
		origExtraFiles:    r.origExtraFiles,
		extraFiles:        maps.Clone(r.origExtraFiles),

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
	// bash marks its own numeric variables `-i`, so `declare -i` with no
	// operands lists eight of them where koi listed none, and every full
	// listing carried `declare -r EUID` against bash's `declare -ir
	// EUID`. The attribute is not only cosmetic: `declare -p` output is
	// meant to be re-evaluable, and a variable listed without `-i`
	// re-reads as one whose later assignments are not arithmetic (#720).
	if !r.writeEnv.Get("UID").IsSet() {
		r.setVar("UID", expand.Variable{
			Set:      true,
			Kind:     expand.String,
			ReadOnly: true,
			Integer:  true,
			Str:      strconv.Itoa(os.Getuid()),
		})
	}
	if !r.writeEnv.Get("EUID").IsSet() {
		r.setVar("EUID", expand.Variable{
			Set:      true,
			Kind:     expand.String,
			ReadOnly: true,
			Integer:  true,
			Str:      strconv.Itoa(os.Geteuid()),
		})
	}
	if !r.writeEnv.Get("PPID").IsSet() {
		// PPID is stored rather than computed on read, which is what
		// gives it the `-ir` bash reports *and* the refusal underneath
		// it: `PPID=1` is bash's "readonly variable" and was accepted
		// silently here, so a script clobbering the name it uses to find
		// its parent was told it worked (#720). The value cannot change
		// during a shell's life, so nothing is lost by recording it.
		r.setVar("PPID", expand.Variable{
			Set:      true,
			Kind:     expand.String,
			ReadOnly: true,
			Integer:  true,
			Str:      strconv.Itoa(os.Getppid()),
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
	r.setVarInt("OPTIND", "1")

	r.dirStack = append(r.dirStack, r.Dir)

	r.importEnvFuncs()

	// SECONDS counts from here, and BASHPID reports the real pid until
	// a subshell takes its own number (#408).
	r.startTime, r.secondsBase = time.Now(), 0
	r.bashPIDValue = os.Getpid()

	r.didReset = true
}

// importEnvFuncs defines the functions the environment carries, which is
// how an exported function reaches a child shell (#387): bash writes
// each one as BASH_FUNC_<name>%%="() { … }" and reads them back here.
// A definition that does not parse is skipped rather than reported —
// this is untrusted input from whoever built the environment, and the
// shell has no business failing to start over it (the shape behind
// CVE-2014-6271, where the parse continued past the definition).
func (r *Runner) importEnvFuncs() {
	const prefix, suffix = "BASH_FUNC_", "%%"
	for name, vr := range r.Env.Each {
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		fname := name[len(prefix) : len(name)-len(suffix)]
		if fname == "" {
			continue
		}
		body := vr.String()
		if !strings.HasPrefix(body, "()") {
			continue
		}
		file, err := syntax.NewParser().Parse(strings.NewReader(fname+" "+body), "")
		if err != nil || len(file.Stmts) != 1 {
			continue
		}
		fn, ok := file.Stmts[0].Cmd.(*syntax.FuncDecl)
		if !ok {
			continue
		}
		if r.Funcs == nil {
			r.Funcs = make(map[string]*syntax.Stmt, 1)
		}
		r.Funcs[fname] = fn.Body
	}
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
	r.runPrologue(ctx)
	ended := false
	switch node := node.(type) {
	case *syntax.File:
		r.filename = node.Name
		r.stmtsTopLevel(ctx, node.Stmts)
		// Running an entire file implies an exit; a statement or command
		// only exits the shell via the exit builtin, errexit, and so on.
		ended = true
	case *syntax.Stmt:
		r.stmt(ctx, node)
	case syntax.Command:
		r.cmd(ctx, node)
	default:
		return fmt.Errorf("node can only be File, Stmt, or Command: %T", node)
	}
	return r.runEpilogue(ctx, ended)
}

// RunStmts runs one chunk of a script which the shell is reading and
// running as it goes — the statements of a single input line — without
// implying that the script has ended (#450, #516).
//
// [Runner.Run] on a whole [*File] is the all-at-once form: it ends the
// script in the same call, firing the EXIT trap. Reading incrementally
// separates the two, so [Runner.Finish] is the other half and must be
// called when the input runs out. name is the script's name, which is
// what BASH_SOURCE reports for the frame.
//
// Like Run, the returned error carries the chunk's exit status, and
// [Runner.Exited] reports whether the shell should stop.
func (r *Runner) RunStmts(ctx context.Context, name string, stmts []*syntax.Stmt) error {
	r.runPrologue(ctx)
	r.filename = name
	r.stmtsTopLevel(ctx, stmts)
	return r.runEpilogue(ctx, false)
}

// Finish ends a script run made of [Runner.RunStmts] calls: it fires any
// pending signal trap and the EXIT trap, and reports the script's status.
//
// It is a no-op beyond reporting that status when the script already
// exited on its own, since the exit trap fired then.
func (r *Runner) Finish(ctx context.Context) error {
	if !r.didReset {
		r.Reset()
	}
	r.fillExpandConfig(ctx)
	// The last chunk's status is the script's, and is what the EXIT trap
	// sees as $?.
	r.exit = r.lastExit
	return r.runEpilogue(ctx, true)
}

// runPrologue is the state every run boundary starts from.
func (r *Runner) runPrologue(ctx context.Context) {
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
}

// runEpilogue closes a run boundary and reports its status. ended says
// the script is over, which is what makes the EXIT trap fire.
func (r *Runner) runEpilogue(ctx context.Context, ended bool) error {
	// A bare Command bypasses stmt, which normally updates lastExit.
	r.lastExit = r.exit
	if ended || r.exit.exiting {
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
// nextBashPID hands each subshell a number of its own. koi runs a
// subshell in the same process, so BASHPID cannot report a real pid —
// what a script tests with it is whether it is in a different
// execution context, and distinct numbers answer that.
var bashPIDCounter atomic.Int32

func nextBashPID() int {
	return int(bashPIDCounter.Add(1)) + os.Getpid()
}

// bashPID reports this context's BASHPID.
func (r *Runner) bashPID() int {
	if r.bashPIDValue == 0 {
		return os.Getpid()
	}
	return r.bashPIDValue
}

// groupsList reports the caller's group ids, which GROUPS publishes.
// bash discards writes to it, so it is read-only here.
func (r *Runner) groupsList() []string {
	gids, err := os.Getgroups()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(gids))
	for _, gid := range gids {
		out = append(out, strconv.Itoa(gid))
	}
	return out
}

func (r *Runner) subshell(background bool) *Runner {
	if !r.didReset {
		r.Reset()
	}
	// Keep in sync with the Runner type. Manually copy fields, to not copy
	// sensitive ones like [errgroup.Group], and to do deep copies of slices.
	r2 := &Runner{
		Dir:         r.Dir,
		startTime:   r.startTime,
		secondsBase: r.secondsBase,
		// Deliberately not the generator: a subshell reads its own
		// numbers, so `$(echo $RANDOM)` does not move the parent's
		// sequence (#547). What a script unset stays unset, though.
		unsetDynamic:     maps.Clone(r.unsetDynamic),
		readDynamic:      maps.Clone(r.readDynamic),
		wroteDynamic:     maps.Clone(r.wroteDynamic),
		bashPIDValue:     nextBashPID(),
		argv0:            r.argv0,
		disabledBuiltins: maps.Clone(r.disabledBuiltins),
		hashTable:        maps.Clone(r.hashTable),
		tempDir:          r.tempDir,
		Params:           r.Params,
		callHandler:      r.callHandler,
		execHandler:      r.execHandler,
		openHandler:      r.openHandler,
		readDirHandler:   r.readDirHandler,
		statHandler:      r.statHandler,
		accessHandler:    r.accessHandler,
		stdin:            r.stdin,
		stdout:           r.stdout,
		stderr:           r.stderr,
		bgWriteMu:        r.bgWriteMu,
		filename:         r.filename,
		opts:             r.opts,
		usedNew:          r.usedNew,
		exit:             r.exit,
		lastExit:         r.lastExit,

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
	// Being *inside* a trap action crosses too (#496). A pipeline stage
	// inherits the DEBUG callback so the whole pipeline is traced, and
	// without this a trap action that is itself a pipeline fires the
	// trap again from its own stages — which produced no output at all,
	// and took the command that triggered the trap with it.
	r2.handlingTrap = r.handlingTrap
	// The trace hook crosses unconditionally: it is the shell's own
	// instrumentation, not a script's trap, so no shell option governs
	// whether a subshell or pipeline stage is traced (#474).
	r2.traceHook = r.traceHook
	// A subshell is still inside whatever function or sourced file
	// contains it, which is what `return` asks about (#422). Losing
	// these made `f(){ (return 5); }` report "can only be done from a
	// func" and, worse, made `$(echo a; return; echo b)` carry on past
	// the return — a wrong status is visible, silently running the rest
	// of a command substitution is not.
	r2.inFunc = r.inFunc
	r2.inSource = r.inSource
	// A subshell is the same frame as far as RETURN is concerned: it is
	// not a function call, so nothing about it changes reachability.
	r2.callbackReturn = r.callbackReturn
	r2.returnTrapOff = r.returnTrapOff
	r2.debugTrapOff = r.debugTrapOff
	r2.listed = r.listed
	r2.sigListed = maps.Clone(r.sigListed)
	r2.sigIgnoredAtEntry = r.sigIgnoredAtEntry
	r2.varHooks = r.varHooks
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
	// A readonly function is still readonly inside a subshell — measured,
	// `readonly -f f; ( f() { …; } )` refuses there too — and a subshell
	// marking one must not mark it in the parent, so the set is cloned
	// alongside the functions it describes (#615).
	r2.readonlyFuncs = maps.Clone(r.readonlyFuncs)
	// The trace attribute follows the same rule, and for the same reason:
	// a subshell's `declare -ft` must not make the parent's function
	// traced (#697).
	r2.tracedFuncs = maps.Clone(r.tracedFuncs)
	r2.Vars = make(map[string]expand.Variable)
	r2.alias = maps.Clone(r.alias)

	r2.dirStack = append(r2.dirBootstrap[:0], r.dirStack...)
	r2.fillExpandConfig(r.ectx)
	r2.didReset = true
	return r2
}
