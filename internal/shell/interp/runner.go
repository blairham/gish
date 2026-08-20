// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package interp

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"maps"
	"math"
	mathrand "math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/shinternal"
	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

const (
	// shellReplyPS3Var, or PS3, is a special variable in Bash used by the select command,
	// while the shell is awaiting for input. the default value is [shellDefaultPS3]
	shellReplyPS3Var = "PS3"
	// shellDefaultPS3, or #?, is PS3's default value
	shellDefaultPS3 = "#? "
	// shellReplyVar, or REPLY, is a special variable in Bash that is used to store the result of
	// the select command or of the read command, when no variable name is specified
	shellReplyVar = "REPLY"
	// shellFuncNameVar names the call stack variable, innermost function first.
	shellFuncNameVar = "FUNCNAME"
	// shellSourceVar and shellLineNoVar are the other two views of the same
	// stack: the file each frame's code lives in, and the line each frame
	// was entered from.
	shellSourceVar = "BASH_SOURCE"
	shellLineNoVar = "BASH_LINENO"
	// shellCommandVar, or BASH_COMMAND, holds the command being run, as
	// written rather than as expanded. Read by a DEBUG trap and, more
	// often, by an ERR trap saying which command failed.
	shellCommandVar = "BASH_COMMAND"

	fifoNamePrefix = "sh-interp-"
)

func (r *Runner) fillExpandConfig(ctx context.Context) {
	r.ectx = ctx
	r.ecfg = &expand.Config{
		Env: expandEnv{r},
		CmdSubst: func(w io.Writer, cs *syntax.CmdSubst) error {
			switch len(cs.Stmts) {
			case 0: // nothing to do
				return nil
			case 1: // $(<file)
				word := catShortcutArg(cs.Stmts[0])
				if word == nil {
					break
				}
				path := r.literal(word)
				f, err := r.open(ctx, path, os.O_RDONLY, 0, true)
				if err != nil {
					return err
				}
				_, err = io.Copy(w, f)
				f.Close()
				return err
			}
			r2 := r.subshell(false)
			// A command substitution sees the caller's jobs. bash
			// draws the line here rather than at "is this a
			// subshell": `echo $(jobs -r | wc -l)` counts them and
			// `( jobs -r | wc -l )` does not, and it is the former
			// that every bounded parallel loop is built out of
			// (#302). They arrive non-waitable; see bgProc.inherited.
			r2.bgProcs = inheritedJobs(r.bgProcs)
			r2.stdout = w
			r2.stmts(ctx, cs.Stmts)
			r2.runSubshellExitTrap(ctx)
			r2.exit.exiting = false  // subshells don't exit the parent shell
			r2.exit.aborting = false // nor unwind it: an abort inside a subshell ends that subshell
			r.lastExpandExit = r2.exit
			if r2.exit.fatalExit {
				return r2.exit.err // surface fatal errors immediately
			}
			return nil
		},
		ProcSubst: func(ps *syntax.ProcSubst) (string, error) {
			if ps.Op == syntax.CmdInTemp { // zsh's =(...)
				return "", fmt.Errorf("unsupported")
			}
			if runtime.GOOS == "windows" {
				return "", fmt.Errorf("TODO: support process substitution on Windows")
			}
			if len(ps.Stmts) == 0 { // nothing to do
				return os.DevNull, nil
			}

			// We can't atomically create a random unused temporary FIFO.
			// Similar to [os.CreateTemp],
			// keep trying new random paths until one does not exist.
			// We use a uint64 because a uint32 easily runs into retries.
			var path string
			try := 0
			for {
				path = filepath.Join(r.tempDir, fifoNamePrefix+strconv.FormatUint(mathrand.Uint64(), 16))
				err := mkfifo(path, 0o666)
				if err == nil {
					break
				}
				if !os.IsExist(err) {
					return "", fmt.Errorf("cannot create fifo: %v", err)
				}
				if try++; try > 100 {
					return "", fmt.Errorf("giving up at creating fifo: %v", err)
				}
			}

			r2 := r.subshell(true)
			stdout := r.origStdout
			// TODO: note that `man bash` mentions that `wait` only waits for the last
			// process substitution as long as it is $!; the logic here would mean we wait for all of them.
			bg := bgProc{
				done: make(chan struct{}),
				exit: new(exitStatus),
			}
			r.bgProcs = append(r.bgProcs, bg)
			go func() {
				defer func() {
					*bg.exit = r2.exit
					close(bg.done)
				}()
				switch ps.Op {
				case syntax.CmdIn:
					f, err := os.OpenFile(path, os.O_WRONLY, 0)
					if err != nil {
						r.errf("cannot open fifo for stdout: %v\n", err)
						return
					}
					r2.stdout = f
					defer func() {
						if err := f.Close(); err != nil {
							r.errf("closing stdout fifo: %v\n", err)
						}
						os.Remove(path)
					}()
				case syntax.CmdOut:
					f, err := os.OpenFile(path, os.O_RDONLY, 0)
					if err != nil {
						r.errf("cannot open fifo for stdin: %v\n", err)
						return
					}
					r2.stdin = f
					r2.stdout = stdout

					defer func() {
						f.Close()
						os.Remove(path)
					}()
				default:
					// Should only happen if we forgot a case above.
					panic(fmt.Sprintf("unexpected process substitution operator: %q", ps.Op))
				}
				r2.stmts(ctx, ps.Stmts)
				r2.runSubshellExitTrap(ctx)
				r2.exit.exiting = false  // subshells don't exit the parent shell
				r2.exit.aborting = false // nor unwind it: an abort inside a subshell ends that subshell
			}()
			return path, nil
		},
	}
	r.updateExpandOpts()
}

// catShortcutArg checks if a statement is of the form "$(<file)". The redirect
// word is returned if there's a match, and nil otherwise.
func catShortcutArg(stmt *syntax.Stmt) *syntax.Word {
	if stmt.Cmd != nil || stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown {
		return nil
	}
	if len(stmt.Redirs) != 1 {
		return nil
	}
	redir := stmt.Redirs[0]
	if redir.Op != syntax.RdrIn {
		return nil
	}
	return redir.Word
}

func (r *Runner) updateExpandOpts() {
	if r.opts[optNoGlob] {
		r.ecfg.ReadDir2 = nil
	} else {
		r.ecfg.ReadDir2 = func(s string) ([]fs.DirEntry, error) {
			return r.readDirHandler(r.handlerCtx(r.ectx, handlerKindReadDir, todoPos), s)
		}
	}
	r.ecfg.GlobStar = r.opts[optGlobStar]
	r.ecfg.DotGlob = r.opts[optDotGlob]
	r.ecfg.NoCaseGlob = r.opts[optNoCaseGlob]
	r.ecfg.NullGlob = r.opts[optNullGlob]
	r.ecfg.NoBraces = !r.opts[optBraceExpand]
	r.ecfg.NoUnset = r.opts[optNoUnset]
	r.ecfg.ExtGlob = r.opts[optExtGlob]
	r.ecfg.FailGlob = r.opts[optFailGlob]
}

func (r *Runner) expandErr(err error) {
	if err == nil {
		return
	}
	errMsg := err.Error()
	fmt.Fprintln(r.stderr, errMsg)
	switch {
	case errors.As(err, &expand.UnsetParameterError{}):
		// nounset is genuinely fatal in bash: measured against 5.3, a
		// script file dies at the unbound variable exactly as -c does,
		// exit 1 both ways.
		r.exit.code = 1
		r.exit.exiting = true
	case strings.HasSuffix(errMsg, "readonly variable"),
		strings.HasSuffix(errMsg, "arithmetic syntax error"),
		strings.HasSuffix(errMsg, "expression recursion level exceeded"):
		// An arithmetic error in an expansion is the same input-unit
		// abandonment (#366): bash's `echo "${x:bad}"` loses the rest
		// of the -c string and a script continues at the next line.
		r.exit.code = 1
		r.exit.aborting = true
	case strings.HasPrefix(errMsg, "no match: "):
		// failglob is the same abandonment shape (#375): -c loses the
		// rest of the string and exits 1, a script file continues at
		// the next line. Measured against 5.3.
		r.exit.code = 1
		r.exit.aborting = true
	case errMsg == "invalid indirect expansion":
		// An invalid indirect is the readonly-assignment shape (#308),
		// not the nounset one: bash abandons the current input unit and
		// goes back to reading, so a script file continues at the next
		// command while -c — one unit — loses its remainder and exits 1.
		// Measured against 5.3; this was `exiting`, which cost a script
		// every line after the failure (nameref3.sub forfeited its
		// second half to one probe on line 29).
		r.exit.code = 1
		r.exit.aborting = true
	}
	// Other cases neither exit nor abort.
}

func (r *Runner) arithm(expr syntax.ArithmExpr) int {
	n, err := expand.Arithm(r.ecfg, expr)
	r.expandErr(err)
	return n
}

func (r *Runner) fields(words ...*syntax.Word) []string {
	strs, err := expand.Fields(r.ecfg, words...)
	r.expandErr(err)
	return strs
}

func (r *Runner) literal(word *syntax.Word) string {
	str, err := expand.Literal(r.ecfg, word)
	r.expandErr(err)
	return str
}

// literalAssign expands an assignment's value, where a tilde also
// expands after each unquoted colon (#364).
func (r *Runner) literalAssign(word *syntax.Word) string {
	str, err := expand.LiteralAssign(r.ecfg, word)
	r.expandErr(err)
	return str
}

func (r *Runner) document(word *syntax.Word) string {
	str, err := expand.Document(r.ecfg, word)
	r.expandErr(err)
	return str
}

func (r *Runner) pattern(word *syntax.Word) string {
	str, err := expand.Pattern(r.ecfg, word)
	r.expandErr(err)
	return str
}

// expandEnviron exposes [Runner]'s variables to the expand package.
type expandEnv struct {
	r *Runner
}

var _ expand.WriteEnviron = expandEnv{}

func (e expandEnv) Get(name string) expand.Variable {
	return e.r.lookupVar(name)
}

func (e expandEnv) Set(name string, vr expand.Variable) error {
	// A readonly write from inside an expansion — $((xx++)), ${x:=v} —
	// is fatal to the input unit in bash (#370): the command aborts with
	// status 1 and a script continues at its next line. export's and a
	// temp-env's violations only cost status 1, so the raise lives on
	// this path rather than in setVar.
	if prev := e.r.writeEnv.Get(name); prev.ReadOnly {
		e.r.errf("%s: readonly variable\n", name)
		e.r.exit.code = 1
		e.r.exit.aborting = true
		return nil
	}
	e.r.setVar(name, vr)
	return nil // TODO: return any errors
}

func (e expandEnv) Each(fn func(name string, vr expand.Variable) bool) {
	e.r.writeEnv.Each(fn)
}

var todoPos syntax.Pos // for handlerCtx callers where we don't yet have a position

func (r *Runner) handlerCtx(ctx context.Context, kind handlerKind, pos syntax.Pos) context.Context {
	hc := HandlerContext{
		runner:         r,
		kind:           kind,
		Env:            &overlayEnviron{parent: r.writeEnv},
		Dir:            r.Dir,
		Pos:            pos,
		Stdout:         r.stdout,
		Stderr:         r.stderr,
		LastExitStatus: int(r.lastExit.code),
	}
	if r.stdin != nil { // do not leave hc.Stdin as a typed nil
		hc.Stdin = r.stdin
	}
	hc.ExtraFiles = r.extraFileSlice()
	return context.WithValue(ctx, handlerCtxKey{}, hc)
}

// extraFileSlice lays the open descriptors above 2 out the way
// [os/exec.Cmd.ExtraFiles] wants them: entry i is descriptor 3+i, and a
// descriptor which is not open is a nil entry, which leaves it closed in the
// child rather than pointing it somewhere.
func (r *Runner) extraFileSlice() []*os.File {
	highest := 2
	for fd := range r.extraFiles {
		if fd > highest {
			highest = fd
		}
	}
	if highest == 2 {
		return nil
	}
	files := make([]*os.File, highest-2)
	for fd, rwc := range r.extraFiles {
		// Only a real file can be handed to another process.
		if f, ok := rwc.(*os.File); ok {
			files[fd-3] = f
		}
	}
	return files
}

// fdWriter returns what is open for writing on a descriptor, or nil.
func (r *Runner) fdWriter(fd int) io.Writer {
	switch fd {
	case 1:
		return r.stdout
	case 2:
		return r.stderr
	}
	if f, ok := r.extraFiles[fd]; ok {
		return f
	}
	return nil
}

// fdReader returns what is open for reading on a descriptor, or nil.
//
// The counterpart to fdWriter, added for `read -u` (#267): descriptors
// above 2 have worked for redirection since #263, so `read x <&3` read the
// right file while `read -u 3 x` — the spelling the manual uses, and the
// one `flock` wrappers write — was refused outright.
func (r *Runner) fdReader(fd int) io.Reader {
	if fd == 0 {
		if r.stdin == nil {
			return nil
		}
		return r.stdin
	}
	if f, ok := r.extraFiles[fd]; ok {
		return f
	}
	return nil
}

// setFdWriter points a descriptor at a writer. A descriptor above 2 has to hold
// something closeable, so a plain writer is wrapped; note that only a real file
// can be handed to another process, which is what [Runner.extraFileSlice] cares
// about.
func (r *Runner) setFdWriter(fd int, w io.Writer) {
	switch fd {
	case 1:
		r.stdout = w
		return
	case 2:
		r.stderr = w
		return
	}
	rwc, ok := w.(io.ReadWriteCloser)
	if !ok {
		rwc = writerFile{w}
	}
	r.setFdFile(fd, rwc)
}

// setFdFile points a descriptor at an open file.
func (r *Runner) setFdFile(fd int, rwc io.ReadWriteCloser) {
	switch fd {
	case 1:
		r.stdout = rwc
		return
	case 2:
		r.stderr = rwc
		return
	}
	if r.extraFiles == nil {
		r.extraFiles = make(map[int]io.ReadWriteCloser)
	}
	r.extraFiles[fd] = rwc
}

// closeFd closes a descriptor, as "N>&-" and "N<&-" do. Note that writes to a
// closed 1 or 2 are discarded rather than failing, which is what those two did
// before any descriptor above 2 existed.
func (r *Runner) closeFd(fd int) {
	switch fd {
	case 0:
		r.stdin = nil
	case 1:
		r.stdout = io.Discard
	case 2:
		r.stderr = io.Discard
	default:
		delete(r.extraFiles, fd)
	}
}

// writerFile adapts a plain writer to sit in the descriptor table.
type writerFile struct{ io.Writer }

func (writerFile) Read([]byte) (int, error) { return 0, io.EOF }
func (writerFile) Close() error             { return nil }

// freeFd returns the lowest descriptor which is not open, starting at 10 as
// bash does for "{varname}>" so that a script's own numbered descriptors are
// left alone.
func (r *Runner) freeFd() int {
	for fd := 10; ; fd++ {
		if _, ok := r.extraFiles[fd]; !ok {
			return fd
		}
	}
}

func (r *Runner) out(s string) {
	io.WriteString(r.stdout, s)
}

func (r *Runner) outf(format string, a ...any) {
	fmt.Fprintf(r.stdout, format, a...)
}

func (r *Runner) errf(format string, a ...any) {
	fmt.Fprintf(r.stderr, format, a...)
}

func (r *Runner) stop(ctx context.Context) bool {
	// Inside a trap action these flags always belong to the action itself
	// — runTrapCallback clears the shell's in-flight ones at entry, which
	// is what lets an EXIT trap run while the shell is exiting — so a
	// `return` raised within the action ends what it should end rather
	// than being suppressed until the action runs out of statements
	// (#355).
	if r.exit.returning || r.exit.exiting || r.exit.aborting {
		return true
	}
	if err := ctx.Err(); err != nil {
		r.exit.fatal(err)
		return true
	}
	if r.opts[optNoExec] {
		return true
	}
	return false
}

func (r *Runner) stmt(ctx context.Context, st *syntax.Stmt) {
	r.runPendingSignalTraps(ctx)
	if r.stop(ctx) {
		return
	}
	r.exit = exitStatus{}
	if st.Background || st.Disown {
		// From here on the shell and this job write to the same stdout
		// concurrently, which is not something a bash background job
		// ever does to it -- see syncWriter (#301).
		r.shareOutput()
		r2 := r.subshell(true)
		st2 := *st
		st2.Background = false
		st2.Disown = false
		bg := bgProc{
			done: make(chan struct{}),
			exit: new(exitStatus),
			cmd:  stmtSource(st),
		}
		r.bgProcs = append(r.bgProcs, bg)
		go func() {
			r2.Run(ctx, &st2)
			r2.runSubshellExitTrap(ctx)
			r2.exit.exiting = false  // subshells don't exit the parent shell
			r2.exit.aborting = false // nor unwind it: an abort inside a subshell ends that subshell
			*bg.exit = r2.exit
			close(bg.done)
		}()
	} else {
		r.stmtSync(ctx, st)
	}
	r.lastExit = r.exit
}

// callFrame is one execution context: a function call, a `source`, or the
// script itself. Innermost first, matching the indexing every reader uses.
//
// The three variables and the `caller` builtin are four views of this one
// stack, and the indexing between them is the part that is easy to get
// subtly wrong, so it is stated once here rather than in each reader:
//
//   - FUNCNAME[i] is frame i's name — a function's name, or the literal
//     "source" or "main" for the other two kinds.
//   - BASH_SOURCE[i] is the file frame i's *code* lives in. For a function
//     that is where it was defined, not where it was called.
//   - BASH_LINENO[i] is the line in frame i+1 that frame i was entered
//     from, so the two are read together: a helper names its caller with
//     BASH_SOURCE[1] and BASH_LINENO[0].
//   - `caller N` prints those three from one frame down: BASH_LINENO[N],
//     FUNCNAME[N+1], BASH_SOURCE[N+1].
type callFrame struct {
	name     string // FUNCNAME
	source   string // BASH_SOURCE
	callLine uint   // BASH_LINENO
	isFunc   bool
}

const (
	sourceFrameName = "source"
	mainFrameName   = "main"
)

// enterFuncForReturnTrap turns the RETURN trap off for a function call
// unless "functrace" is set, and returns what to restore on the way out.
//
// bash inherits RETURN into a function only under `set -T`, which is the
// same switch that governs DEBUG. `source` is deliberately not routed
// through here: a sourced file inherits the trap unconditionally, so its
// return fires whatever the caller had set.
func (r *Runner) enterFuncForReturnTrap() bool {
	old := r.returnTrapOff
	if !r.opts[optFuncTrace] {
		r.returnTrapOff = true
	}
	return old
}

// runReturnTrap fires the RETURN trap for a frame that is about to be
// left, if it is set and reachable from here.
//
// Called *before* the frame is popped, because FUNCNAME inside the trap
// is still the function that is returning — which is what a cleanup
// handler reads to name what it is cleaning up after. The exit status is
// restored by trapCallback, so a `return 5` still answers 5.
func (r *Runner) runReturnTrap(ctx context.Context) {
	if r.callbackReturn == "" || r.returnTrapOff {
		return
	}
	r.trapCallback(ctx, r.callbackReturn, "return", r.callbackReturnLine)
}

// pushFrame enters a context; the returned function leaves it.
func (r *Runner) pushFrame(f callFrame) func() {
	old := r.frames
	r.frames = append([]callFrame{f}, r.frames...)
	return func() { r.frames = old }
}

// inFunction reports whether the innermost context is a function call,
// which is what decides whether FUNCNAME exists at all.
func (r *Runner) inFunction() bool {
	return len(r.frames) > 0 && r.frames[0].isFunc
}

// baseFrames returns the stack with the script's own frame at the bottom.
//
// It is appended on read rather than pushed at startup because the
// interpreter has no single point where a run begins and ends — Run can be
// called repeatedly on the same runner — and a bottom frame pushed once
// would have to be un-pushed by something.
func (r *Runner) baseFrames() []callFrame {
	if r.mainScript == "" {
		return r.frames
	}
	return append(slices.Clone(r.frames), callFrame{
		name:   mainFrameName,
		source: r.mainScript,
	})
}

// funcNameVar answers FUNCNAME. bash leaves it unset rather than empty
// outside a function, so a script can tell "not in a function" from a
// function whose name happens to be empty — and every `${FUNCNAME[1]:-…}`
// helper depends on that distinction.
func (r *Runner) funcNameVar() expand.Variable {
	if !r.inFunction() {
		return expand.Variable{}
	}
	frames := r.baseFrames()
	names := make([]string, len(frames))
	for i, f := range frames {
		names[i] = f.name
	}
	return expand.Variable{Set: true, Kind: expand.Indexed, List: names}
}

// sourceVar answers BASH_SOURCE, which unlike FUNCNAME is set whenever
// there is any context at all — at the top level of a script it holds
// that script, which is what the `[[ ${BASH_SOURCE[0]} == $0 ]]`
// sourced-or-executed idiom reads.
func (r *Runner) sourceVar() expand.Variable {
	frames := r.baseFrames()
	if len(frames) == 0 {
		return expand.Variable{}
	}
	files := make([]string, len(frames))
	for i, f := range frames {
		files[i] = f.source
	}
	return expand.Variable{Set: true, Kind: expand.Indexed, List: files}
}

func (r *Runner) lineNoVar() expand.Variable {
	frames := r.baseFrames()
	if len(frames) == 0 {
		return expand.Variable{}
	}
	lines := make([]string, len(frames))
	for i, f := range frames {
		lines[i] = strconv.FormatUint(uint64(f.callLine), 10)
	}
	return expand.Variable{Set: true, Kind: expand.Indexed, List: lines}
}

// shellPipeStatusVar holds the exit status of each stage of the last pipeline.
const shellPipeStatusVar = "PIPESTATUS"

// setPipeStatus records the statuses of the command which just ran, unless a
// nested statement already did so for the same status; see pipeStatusSet.
func (r *Runner) setPipeStatus() {
	if r.pipeStatusSet {
		return
	}
	r.pipeStatusSet = true
	codes := r.pipeStatus
	if codes == nil {
		codes = []uint8{r.exit.code}
	}
	list := make([]string, len(codes))
	for i, code := range codes {
		list[i] = strconv.Itoa(int(code))
	}
	r.setVar(shellPipeStatusVar, expand.Variable{
		Set:  true,
		Kind: expand.Indexed,
		List: list,
	})
}

func (r *Runner) stmtSync(ctx context.Context, st *syntax.Stmt) {
	// This statement's status is not one the ERR trap has run for yet, nor one
	// PIPESTATUS has been set for. Note that we clear before running the
	// command, so a nested statement which does either still suppresses the
	// compound commands above it.
	r.errTrapFired = false
	r.pipeStatusSet = false
	r.pipeStatus = nil

	if r.traceCommand(ctx, st) {
		// extdebug: the DEBUG trap cancelled this command; nothing ran,
		// not even its redirections, and $? reads 0 (#355).
		return
	}

	oldIn, oldOut, oldErr := r.stdin, r.stdout, r.stderr
	// The descriptor table is modified in place, so a statement with its own
	// redirections gets a copy to modify and the original is put back after.
	oldExtraFiles := r.extraFiles
	if len(st.Redirs) > 0 {
		r.extraFiles = maps.Clone(r.extraFiles)
	}
	var closers []io.Closer
	for _, rd := range st.Redirs {
		cls, err := r.redir(ctx, rd)
		if err != nil {
			r.exit.code = 1
			break
		}
		if cls != nil {
			closers = append(closers, cls)
		}
	}
	if r.exit.ok() && st.Cmd != nil {
		r.cmd(ctx, st.Cmd)
	}
	// Note that this must come before the negation below, as PIPESTATUS holds
	// the statuses the commands actually exited with; "! false" leaves $? as 0
	// but PIPESTATUS as 1.
	r.setPipeStatus()
	if st.Negated {
		if r.exit.ok() {
			r.exit.code = 1
		} else {
			r.exit.clear()
		}
	} else if b, ok := st.Cmd.(*syntax.BinaryCmd); ok && (b.Op == syntax.AndStmt || b.Op == syntax.OrStmt) {
	} else if !r.exit.ok() && !r.noErrExit {
		// Run the ERR trap for the command which failed, but not again for each
		// compound command which merely propagates its status outwards; bash
		// runs it once per level. Below the top level it only runs at all when
		// "errtrace" is set.
		if !r.errTrapFired && (r.errTrapDepth == 0 || r.opts[optErrTrace]) {
			r.errTrapFired = true
			r.trapCallback(ctx, r.callbackErr, "error", st.Pos().Line())
		}
		// If the "errexit" option is set and a command failed, exit the shell. Exceptions:
		//
		//   conditions (if <cond>, while <cond>, etc)
		//   part of && or || lists; excluded via "else" above
		//   preceded by !; excluded via "else" above
		if r.opts[optErrExit] {
			r.exit.exiting = true
		}
	}
	if r.keepRedirs {
		// The exec builtin made this statement's redirections apply to the
		// shell itself, so don't undo them and keep their files open.
		r.keepRedirs = false
	} else if len(st.Redirs) > 0 {
		r.stdin, r.stdout, r.stderr = oldIn, oldOut, oldErr
		r.extraFiles = oldExtraFiles
		for _, cls := range closers {
			cls.Close()
		}
	}
}

func (r *Runner) cmd(ctx context.Context, cm syntax.Command) {
	if r.stop(ctx) {
		return
	}

	tracingEnabled := r.opts[optXTrace]
	trace := r.tracer()

	switch cm := cm.(type) {
	case *syntax.Block:
		r.stmts(ctx, cm.Stmts)
	case *syntax.Subshell:
		r2 := r.subshell(false)
		r2.stmts(ctx, cm.Stmts)
		r2.runSubshellExitTrap(ctx)
		r2.exit.exiting = false  // subshells don't exit the parent shell
		r2.exit.aborting = false // nor unwind it: an abort inside a subshell ends that subshell
		r.exit = r2.exit
	case *syntax.CallExpr:
		// Build new slices, to not modify the caller's AST
		// nor the slices in the alias map.
		args := cm.Args
		for i := 0; i < len(args); {
			if !r.opts[optExpandAliases] {
				break
			}
			als, ok := r.alias[args[i].Lit()]
			if !ok {
				break
			}
			args = slices.Concat(args[:i], als.args, args[i+1:])
			if !als.blank {
				break
			}
			i += len(als.args)
		}
		r.lastExpandExit = exitStatus{}
		fields := r.fields(args...)
		if len(fields) == 0 {
			for _, as := range cm.Assigns {
				name := as.Name.Value

				prev := r.lookupVar(name)
				// An assignment through a nameref updates the *target*, so
				// both halves must resolve before anything reads prev: the
				// write env already resolved the name, but prev stayed the
				// nameref's own value, so `r[2]=42` through a nameref
				// started from an empty array and replaced the target with
				// a single element, and `r+=x` appended to nothing (#277).
				if n, v := prev.Resolve(r.writeEnv); n != "" {
					name, prev = n, v
				}
				// Here we have a naked "foo=bar", so if we inherited a local var from a parent
				// function we want to signal that we are modifying the parent var rather than
				// creating a new local var via "local foo=bar".
				// TODO: there is likely a better way to do this.
				prev.Local = false

				if prev.ReadOnly {
					// A plain assignment to a readonly variable is fatal in
					// bash, unlike the same error from a command prefix,
					// "declare", "export", "let", "((...))" or "read", which
					// all carry on. Inside a subshell it ends the subshell
					// only, which falls out of how exiting is handled.
					//
					// "Fatal" is bash's word for throwing away the command
					// being run and going back to reading input, though --
					// not for killing the shell, which it only does under
					// `set -o posix`. Marking this `exiting` cost a script
					// every line after the offending one, so a cleanup or a
					// teardown trap below it never ran (#308).
					r.errf("%s: readonly variable\n", name)
					r.exit.code = 1
					r.exit.aborting = true
					return
				}

				name, vr := r.assignVal(name, prev, as, "")
				r.setVarWithIndex(prev, name, as.Index, vr)

				if !tracingEnabled {
					continue
				}

				// Strangely enough, it seems like Bash prints original
				// source for arrays, but the expanded value otherwise.
				// TODO: add test cases for x[i]=y and x+=y.
				if as.Array != nil {
					trace.expr(as)
				} else if as.Value != nil {
					val, err := syntax.Quote(vr.String(), syntax.LangBash)
					if err != nil { // should never happen
						panic(err)
					}
					trace.stringf("%s=%s", name, val)
				}
				trace.newLineFlush()
			}
			// If interpreting the last expansion like $(foo) failed,
			// and the expansion and assignments otherwise succeeded,
			// we need to surface that last exit code.
			if r.exit.ok() {
				r.exit = r.lastExpandExit
			}
			break
		}

		type restoreVar struct {
			name string
			vr   expand.Variable
		}
		var restores []restoreVar

		for _, as := range cm.Assigns {
			name := as.Name.Value
			prev := r.lookupVar(name)
			// Resolve any nameref so we can restore the original final value later on.
			if n, v := prev.Resolve(r.writeEnv); n != "" {
				name, prev = n, v
			}

			name, vr := r.assignVal(name, prev, as, "")
			// Inline command vars are always exported.
			vr.Exported = true

			restores = append(restores, restoreVar{name, prev})

			r.setVar(name, vr)
		}

		trace.call(fields[0], fields[1:]...)
		trace.newLineFlush()

		r.call(ctx, cm.Args[0].Pos(), fields)
		for _, restore := range restores {
			if restore.vr.ReadOnly {
				// The assignment failed and was already reported, so there is
				// nothing to put back and trying would report it a second time.
				continue
			}
			r.setVar(restore.name, restore.vr)
		}
	case *syntax.BinaryCmd:
		switch cm.Op {
		case syntax.AndStmt, syntax.OrStmt:
			oldNoErrExit := r.noErrExit
			r.noErrExit = true
			r.stmt(ctx, cm.X)
			r.noErrExit = oldNoErrExit
			if r.exit.ok() == (cm.Op == syntax.AndStmt) {
				r.stmt(ctx, cm.Y)
			}
		case syntax.Pipe, syntax.PipeAll:
			pr, pw, err := os.Pipe()
			if err != nil {
				r.exit.fatal(err) // not being able to create a pipe is rare but critical
				return
			}
			// A pipeline runs its stages concurrently, so they share
			// stdout and stderr the same way a background job does --
			// `{ echo a >&2; } | { echo b >&2; }` has two stages
			// writing one stderr at once, with no `&` anywhere to
			// hint at it.
			r.shareOutput()
			r2 := r.subshell(true)
			// Every stage of a pipeline is traced, whether or not
			// "functrace" is set: bash reports `echo a` and `cat` alike for
			// `echo a | cat`. A stage is a subshell here as an
			// implementation detail, and letting subshell's inheritance rule
			// apply to it would trace the last stage and silently skip the
			// rest — a partial trace being worse than none, since it reads
			// as a pipeline with fewer commands in it than it has.
			r2.callbackDebug = r.callbackDebug
			// Same exception, for the same reason: `jobs` in a
			// pipeline stage reports the shell's jobs in bash, and
			// the idiom this exists for is a pipeline --
			// `$(jobs -r | wc -l)`. Letting subshell's rule apply
			// would empty the list in the one place it is read
			// (#302). Non-waitable; see bgProc.inherited.
			r2.bgProcs = inheritedJobs(r.bgProcs)
			r2.stdout = pw
			if cm.Op == syntax.PipeAll {
				r2.stderr = pw
			} else {
				r2.stderr = r.stderr
			}
			var wg sync.WaitGroup
			wg.Go(func() {
				r2.stmt(ctx, cm.X)
				r2.runSubshellExitTrap(ctx)
				r2.exit.exiting = false  // subshells don't exit the parent shell
				r2.exit.aborting = false // nor unwind it: an abort inside a subshell ends that subshell
				pw.Close()
			})
			r.pipeStatus = nil
			var r3 *Runner
			if r.opts[optLastPipe] {
				// lastpipe: the final stage runs in the current shell, so
				// `cmd | read x` keeps x. Until #277 this was the only
				// behavior, which answered the most famous bash gotcha
				// un-bash-ly by default.
				oldIn := r.stdin
				r.stdin = pr
				r.stmt(ctx, cm.Y)
				r.stdin = oldIn
			} else {
				// bash's default: the final stage is a subshell like every
				// other stage — its variables, cd, and even `exit` end with
				// it (`echo | exit 3; echo after` prints after). The same
				// two inheritance exceptions as cm.X's subshell apply, for
				// the same reasons. Non-background, unlike cm.X's: this
				// stage runs synchronously in the parent's goroutine, and
				// the background flavor snapshots the environment via Each,
				// which loses values a FuncEnviron can only answer by name.
				r3 = r.subshell(false)
				r3.callbackDebug = r.callbackDebug
				r3.bgProcs = inheritedJobs(r.bgProcs)
				r3.stdin = pr
				r3.stmt(ctx, cm.Y)
				r3.runSubshellExitTrap(ctx)
				r3.exit.exiting = false
				r3.exit.aborting = false
				r.pipeStatus = r3.pipeStatus
				r.exit = r3.exit
			}
			pr.Close()
			wg.Wait()
			// A process substitution started inside a stage — `tee >(…)`
			// — registers on that stage's runner, which the pipeline
			// throws away; the following `wait` then had nothing to wait
			// on and the substitution's output raced the next command
			// (#459). The stages' own jobs come back to the shell that
			// ran the pipeline; the inherited prefix stays the parent's.
			for _, stage := range []*Runner{r2, r3} {
				if stage == nil {
					continue
				}
				for _, bg := range stage.bgProcs {
					if !bg.inherited {
						r.bgProcs = append(r.bgProcs, bg)
					}
				}
			}
			// A pipeline of three or more stages nests to the left, so cm.X is
			// itself a pipeline whose stages r2 collected; only its last stage
			// runs here as cm.Y.
			left := r2.pipeStatus
			if left == nil {
				left = []uint8{r2.exit.code}
			}
			right := r.pipeStatus
			if right == nil {
				right = []uint8{r.exit.code}
			}
			codes := make([]uint8, 0, len(left)+len(right))
			codes = append(append(codes, left...), right...)
			r.pipeStatus = codes
			// The last stage published its own status as it finished, but the
			// pipeline as a whole is the command PIPESTATUS describes, so let
			// this statement publish the full list over the top of it.
			r.pipeStatusSet = false
			if r.opts[optPipeFail] && !r2.exit.ok() && r.exit.ok() {
				r.exit = r2.exit
			}
			if r2.exit.fatalExit {
				r.exit.fatal(r2.exit.err) // surface fatal errors immediately
			}
		}
	case *syntax.IfClause:
		oldNoErrExit := r.noErrExit
		r.noErrExit = true
		r.stmts(ctx, cm.Cond)
		r.noErrExit = oldNoErrExit

		if r.exit.ok() {
			r.stmts(ctx, cm.Then)
			break
		}
		r.exit.clear()
		if cm.Else != nil {
			r.cmd(ctx, cm.Else)
		}
	case *syntax.WhileClause:
		for !r.stop(ctx) {
			oldNoErrExit := r.noErrExit
			r.noErrExit = true
			r.stmts(ctx, cm.Cond)
			r.noErrExit = oldNoErrExit

			stop := r.exit.ok() == cm.Until
			r.exit.clear()
			if stop || r.loopStmtsBroken(ctx, cm.Do) {
				break
			}
		}
	case *syntax.ForClause:
		switch y := cm.Loop.(type) {
		case *syntax.WordIter:
			name := y.Name.Value
			items := r.Params // for i; do ...

			inToken := y.InPos.IsValid()
			if inToken {
				items = r.fields(y.Items...) // for i in ...; do ...
			}

			if cm.Select {
				ps3 := cmp.Or(r.envGet(shellReplyPS3Var), shellDefaultPS3)

				for menu := true; ; {
					if menu {
						// display menu
						for i, word := range items {
							r.errf("%d) %v\n", i+1, word)
						}
						menu = false
					}
					r.errf("%s", ps3)

					line, _, err := r.readLine(ctx, r.stdin, true, '\n', -1, false, 0)
					if err != nil {
						r.errf("\n")
						r.exit.code = 1
						break
					}
					if len(line) == 0 {
						menu = true // no reply; show the menu again
						continue
					}

					reply := string(line)
					r.setVarString(shellReplyVar, reply)

					if c, _ := strconv.Atoi(reply); c > 0 && c <= len(items) {
						r.setVarString(name, items[c-1])
					} else {
						r.setVarString(name, "")
					}

					// execute commands until break or return is encountered
					if r.loopStmtsBroken(ctx, cm.Do) {
						break
					}
				}
				break
			}

			for _, field := range items {
				r.setVarString(name, field)
				trace.stringf("for %s in", y.Name.Value)
				if inToken {
					for _, item := range y.Items {
						trace.string(" ")
						trace.expr(item)
					}
				} else {
					trace.string(` "$@"`)
				}
				trace.newLineFlush()
				if r.loopStmtsBroken(ctx, cm.Do) {
					break
				}
			}
		case *syntax.CStyleLoop:
			if y.Init != nil {
				r.arithm(y.Init)
			}
			// A failing body command does not end the loop (#369):
			// `for ((f=0; f<3; f++)); do …; false; done` runs all three
			// iterations in bash, and ((i++)) from zero answers status 1
			// on its first step, which used to stop a loop whose update
			// lived in its body. Only control flow ends it early — and
			// an arithmetic error in the update, which raises exactly
			// that.
			for y.Cond == nil || r.arithm(y.Cond) != 0 {
				if r.loopStmtsBroken(ctx, cm.Do) {
					break
				}
				if y.Post != nil {
					r.arithm(y.Post)
				}
				if r.exit.exiting || r.exit.returning || r.exit.aborting {
					break
				}
			}
		}
	case *syntax.FuncDecl:
		if cm.Name == nil { // e.g. zsh's anonymous or multi-name functions
			r.errf("unsupported\n")
			r.exit.code = 1
			break
		}
		r.setFunc(cm.Name.Value, cm.Body)
	case *syntax.ArithmCmd:
		r.exit.oneIf(r.arithm(cm.X) == 0)
	case *syntax.LetClause:
		var val int
		for _, expr := range cm.Exprs {
			val = r.arithm(expr)

			if !tracingEnabled {
				continue
			}

			switch expr := expr.(type) {
			case *syntax.Word:
				qs, err := syntax.Quote(r.literal(expr), syntax.LangBash)
				if err != nil {
					return
				}
				trace.stringf("let %v", qs)
			case *syntax.BinaryArithm, *syntax.UnaryArithm:
				trace.expr(cm)
			case *syntax.ParenArithm:
				// TODO
			}
		}

		trace.newLineFlush()
		r.exit.oneIf(val == 0)
	case *syntax.CaseClause:
		trace.string("case ")
		trace.expr(cm.Word)
		trace.string(" in")
		trace.newLineFlush()
		str := r.literal(cm.Word)
		// fallThrough is set by ";&", which runs the next item's statements
		// without testing its patterns at all.
		fallThrough := false
		for _, ci := range cm.Items {
			matched := fallThrough
			if !matched {
				for _, word := range ci.Patterns {
					pattern := r.pattern(word)
					if match(pattern, str) {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
			r.stmts(ctx, ci.Stmts)
			fallThrough = false
			switch ci.Op {
			case syntax.Fallthrough:
				fallThrough = true
			case syntax.Resume, syntax.ResumeKorn:
				// keep testing the patterns which follow
			default: // syntax.Break
				return
			}
		}
	case *syntax.TestClause:
		if r.bashTest(ctx, cm.X, false) == "" && r.exit.ok() {
			// to preserve exit status code 2 for regex errors, etc
			r.exit.code = 1
		}
	case *syntax.DeclClause:
		r.declClause(cm.Variant.Value, cm.Args)
	case *syntax.CoprocClause:
		r.coproc(ctx, cm)
	case *syntax.TimeClause:
		start := time.Now()
		if cm.Stmt != nil {
			r.stmt(ctx, cm.Stmt)
		}
		format := "%s\t%s\n"
		if cm.PosixFormat {
			format = "%s %s\n"
		} else {
			r.outf("\n")
		}
		real := time.Since(start)
		r.outf(format, "real", elapsedString(real, cm.PosixFormat))
		// TODO: can we do these?
		r.outf(format, "user", elapsedString(0, cm.PosixFormat))
		r.outf(format, "sys", elapsedString(0, cm.PosixFormat))
	default:
		// Should only happen if we forgot a case above.
		r.errf("unhandled command node: %T\n", cm)
		r.exit.code = 1
	}
}

// declClause implements declare/typeset/local/export/readonly/nameref.
// It serves two callers: the DeclClause node the parser produces when the
// word sits at command position, and the builtin dispatch, for the call
// form the parser produces when a prefix assignment keeps the word from
// being a keyword -- `ref=xxx typeset -p ref` reached "unsupported
// builtin" while `typeset -p ref` worked (#277).
func (r *Runner) declClause(variant string, args []*syntax.Assign) {
	local, global := false, false
	var modes []string
	valType := ""
	declQuery := "" // "-f", "-F" or "-p" for query mode
	namedAny := false
	unref := false // "+n": detach a nameref
	switch variant {
	case "declare":
		// When used in a function, "declare" acts as "local"
		// unless the "-g" option is used.
		local = r.inFunc
	case "local":
		if !r.inFunc {
			r.errf("local: can only be used in a function\n")
			r.exit.code = 1
			return
		}
		local = true
	case "export":
		modes = append(modes, "-x")
	case "readonly":
		modes = append(modes, "-r")
	case "nameref":
		valType = "-n"
	}
assignLoop:
	for as := range r.flattenAssigns(args) {
		fp := flagParser{remaining: []string{as.Name.Value}}
		// Note that this consumes every flag clustered into the one
		// argument before moving on; "declare -ri" is -r and -i, and
		// stopping after the first silently dropped the rest.
		sawFlag := fp.more()
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-x", "-r", "-i", "+i":
				modes = append(modes, flag)
			case "-a", "-A", "-n":
				valType = flag
			case "+n":
				unref = true
			case "-g":
				global = true
			case "-f", "-p", "-F":
				declQuery = flag
			default:
				r.errf("%s: invalid option %q\n", variant, flag)
				r.exit.code = 2
				return
			}
		}
		if sawFlag {
			continue assignLoop
		}
		name := as.Name.Value
		namedAny = true
		// The builtin fallback path hands the whole word over as the
		// name, so `declare f[qux]=v` arrives with the subscript still
		// attached; the parse path delivers it as as.Index already.
		if as.Index == nil {
			if base, sub, ok := cutElemSubscript(name); ok {
				name = base
				as.Index = &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: sub}}}
			}
		}
		if !syntax.ValidName(name) {
			r.errf("%s: invalid name %q\n", variant, name)
			r.exit.code = 1
			return
		}
		if declQuery == "-F" {
			// declare -F name: print the name alone, which is how a
			// harness enumerates functions without their bodies. Bash
			// returns 1 for a missing function and carries on with the
			// names which follow.
			if r.Funcs[name] != nil {
				r.outf("%s\n", name)
			} else {
				r.exit.code = 1
			}
			continue
		}
		if declQuery == "-f" {
			// declare -f name: print function definition.
			// Bash silently returns exit 1 for missing functions.
			if body := r.Funcs[name]; body != nil {
				r.printFuncDef(name, body)
			} else {
				r.exit.code = 1
			}
			continue
		}
		if declQuery == "-p" {
			// declare -p name: print variable with attributes.
			vr := r.lookupVar(name)
			if !vr.Declared() {
				r.errf("%s: %s: not found\n", variant, name)
				r.exit.code = 1
				continue
			}
			flags := vr.Flags()
			if flags == "" {
				flags = "-"
			}
			switch vr.Kind {
			case expand.Indexed:
				// Declared but never set prints bare — `declare -a c`
				// answers `declare -a c`, not `=()` (#378).
				if !vr.Set {
					r.outf("declare -%s %s\n", flags, name)
					continue
				}
				r.outf("declare -%s %s=(", flags, name)
				for i, v := range vr.List {
					if i > 0 {
						r.out(" ")
					}
					idx := i
					if vr.Indexes != nil {
						idx = vr.Indexes[i]
					}
					r.outf("[%d]=%q", idx, v)
				}
				r.out(")\n")
			case expand.Associative:
				if !vr.Set {
					r.outf("declare -%s %s\n", flags, name)
					continue
				}
				// Keys are sorted for determinism where bash prints its
				// hash order, and each element carries bash's trailing
				// space: ([one]="1" [two]="2" ).
				r.outf("declare -%s %s=(", flags, name)
				for _, k := range slices.Sorted(maps.Keys(vr.Map)) {
					r.outf("[%s]=%q ", k, vr.Map[k])
				}
				r.out(")\n")
			default:
				// Declared but never set prints bare: `declare -n foo`
				// and `declare -x foo`, not `... foo=""`. The two are
				// the same rule, and it turns on Set rather than on
				// the value being empty — `foo=` *is* set, and bash
				// prints `declare -- foo=""` for it.
				if !vr.Set {
					r.outf("declare -%s %s\n", flags, name)
					continue
				}
				r.outf("declare -%s %s=%q\n", flags, name, vr.Str)
			}
			continue
		}
		if unref {
			r.unsetNameRef(name, as)
			continue
		}
		vr := r.lookupVar(name)
		if global {
			// -g reads and writes the global scope through any local
			// shadowing the name (#379): with f's `local v` in scope,
			// g's `declare -g v=two` sets the global and leaves the
			// local — and $v in both functions — untouched.
			vr = r.globalVar(name)
		}
		// The string form of a compound assignment (#379): a value that
		// arrived through expansion as "( ... )" is parsed as an array
		// literal whose elements are then expanded — but only when an
		// explicit -a/-A asks for an array or the variable already is
		// one; bash 5.1 made the bare form stay a literal string, and
		// an unbalanced "(" stays literal too. The expansion is reused
		// either way so a command substitution in the value runs once.
		if as.Value != nil && as.Index == nil &&
			(valType == "-a" || valType == "-A" ||
				vr.Kind == expand.Indexed || vr.Kind == expand.Associative) {
			val := r.literalAssign(as.Value)
			lit := &syntax.Assign{Name: as.Name, Append: as.Append}
			if arr := parseCompoundArray(val); arr != nil {
				lit.Array = arr
			} else {
				lit.Value = &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: val}}}
			}
			as = lit
		}
		// An explicit -a or -A settles the kind before any value is
		// assigned: the attribute is sticky (#378), so `declare -a c`
		// declares an unset array that a later c=4 fills at element 0,
		// a scalar's value carries to element 0, and converting one
		// array kind to the other is bash's error with the data kept.
		if valType == "-a" || valType == "-A" {
			if !r.applyArrayKind(&vr, valType, variant, name) {
				continue
			}
		}
		// prev is what setVarWithIndex consults to route a scalar
		// assignment into an array variable's element 0.
		prev := vr
		// The integer attribute has to be settled before the value is
		// computed, since it is what decides whether the value is an
		// arithmetic expression; the other modes are applied afterwards.
		if slices.Contains(modes, "-i") {
			vr.Integer = true
		} else if slices.Contains(modes, "+i") {
			vr.Integer = false
		}
		if as.Naked {
			switch valType {
			case "-a", "-A":
				// The kind is already applied; the value stands.
			case "-n":
				// `declare -n foo` with no value *promotes* an
				// existing variable: bash reads foo's current value
				// as the name it now points at, so
				//
				//	bar=one; foo=bar; declare -n foo; echo $foo
				//
				// prints "one". Keeping the value and not the
				// attribute — which is what KeepValue did here — left
				// foo an ordinary variable holding the string "bar",
				// so every later read gave the target's *name* where
				// bash gives its value (#277).
				vr.Kind = expand.NameRef
			default:
				vr.Kind = expand.KeepValue
			}
		} else {
			name, vr = r.assignVal(name, vr, as, valType)
		}
		if global {
			vr.Local = false
			vr.Global = true
		} else if local {
			vr.Local = true
		}
		for _, mode := range modes {
			switch mode {
			case "-x":
				vr.Exported = true
			case "-r":
				vr.ReadOnly = true
			case "-i":
				vr.Integer = true
			case "+i":
				vr.Integer = false
			}
		}
		if as.Naked {
			r.setVar(name, vr)
		} else {
			// The index route covers both an explicit subscript —
			// `declare f[qux]=assigned` adds an element rather than
			// flattening the array to a scalar (#378) — and the
			// implicit element 0 a scalar assignment to an array
			// variable lands in.
			r.setVarWithIndex(prev, name, as.Index, vr)
		}
	}
	if !namedAny && valType == "-n" && declQuery == "" && !unref {
		// A bare `declare -n` lists the namerefs, the way a bare
		// `declare -f` lists the functions. Sorted, as bash sorts.
		var names []string
		r.writeEnv.Each(func(name string, vr expand.Variable) bool {
			if vr.Kind == expand.NameRef {
				names = append(names, name)
			}
			return true
		})
		slices.Sort(names)
		for _, name := range names {
			r.outf("declare -n %s=%q\n", name, r.lookupVar(name).Str)
		}
	}
	if !namedAny && (declQuery == "-f" || declQuery == "-F") {
		// A bare "declare -f" or "declare -F" lists every function, sorted
		// by name as bash does. Claude Code's shell snapshot uses the
		// latter to carry the user's functions into an agent subshell.
		names := make([]string, 0, len(r.Funcs))
		for name := range r.Funcs {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if declQuery == "-F" {
				r.outf("declare -f %s\n", name)
			} else {
				r.printFuncDef(name, r.Funcs[name])
			}
		}
	}
}

// printFuncDef prints a function's definition, as "declare -f" does. Note that
// the layout differs from bash's, which indents the body over several lines.
func (r *Runner) printFuncDef(name string, body *syntax.Stmt) {
	r.outf("%s()\n", name)
	printer := syntax.NewPrinter()
	var buf bytes.Buffer
	printer.Print(&buf, body)
	r.outf("%s\n", buf.String())
}

// traceCommand publishes BASH_COMMAND and fires the DEBUG trap before a
// command runs (#268).
//
// A DEBUG trap used to be refused outright here, which koi papered over
// by intercepting `trap … DEBUG` at the call seam and running it once per
// interactive *line*. That left script and -c sessions recording a trap
// and never firing it: accepted, silent, exit 0 — the worst shape, since
// a preexec hook that never runs looks like a shell where nothing
// happens rather than like a shell that refused.
//
// BASH_COMMAND is published for the ERR trap's sake as much as DEBUG's:
// `trap 'echo failed: $BASH_COMMAND' ERR` is the standard way a script
// says which command failed, and it was reporting an empty string. It is
// maintained only while some trap is set, which is the one deliberate
// divergence in it — bash keeps the variable current unconditionally,
// and doing that here would cost printing the statement back to source
// on every command in every loop, to serve a reader that only exists
// inside a trap.
//
// What is matched, measured against bash rather than reasoned about:
// firing before each simple command including every stage of a pipeline
// and a bare assignment, BASH_COMMAND as the *unexpanded* source, and a
// function body or a sourced file left untraced unless "functrace" is
// set. Two known divergences, stated so they are not surprises: bash
// also fires for the header of a `for` or `while`, once per iteration,
// and koi does not; and koi's pipeline stages run concurrently, so their
// traces can interleave where bash's are strictly left to right.
// traceCommand reports whether the statement should be skipped: under
// extdebug, a DEBUG trap answering nonzero cancels the command (#355).
func (r *Runner) traceCommand(ctx context.Context, st *syntax.Stmt) bool {
	if r.handlingTrap {
		return false
	}
	if r.callbackDebug == "" && r.callbackErr == "" && r.callbackExit == "" {
		return false
	}
	// Only a simple command. A compound statement's own trace is the
	// divergence noted above, and firing for both would report every
	// command twice — once for the `if` and once for its body.
	if _, ok := st.Cmd.(*syntax.CallExpr); !ok {
		return false
	}
	r.setVarString(shellCommandVar, stmtSource(st))
	if r.callbackDebug == "" {
		return false
	}
	// A function body and a sourced file are both traced only under
	// "functrace" — the same rule, and both measured: bash prints nothing
	// for the commands inside `. file` until `set -T`.
	if (r.inFunction() || r.inSource) && !r.opts[optFuncTrace] {
		return false
	}
	code := r.trapCallback(ctx, r.callbackDebug, "debug", st.Pos().Line())
	// Under extdebug, a DEBUG trap answering nonzero tells the shell not
	// to run the command at all — the mechanism a debugger's step and
	// skip are built on (#355). The skipped command leaves $? as 0,
	// measured against 5.3.
	return code != 0 && r.opts[optExtDebug]
}

// listedTraps is what `trap -p` reports, as distinct from what runs. See
// the field on [Runner] for why the two are not the same set.
type listedTraps struct{ exit, err, debug, ret string }

// printTraps answers `trap -p` and bare `trap`: the handlers that are
// set, spelled as the commands that would restore them.
//
// That spelling is the whole point of the builtin — a script saves a
// handler with `old=$(trap -p EXIT)` and restores it later by running
// what it captured — so the action is *shell*-quoted rather than
// Go-quoted. `%q` was close enough to read and wrong to re-run: it
// renders a backslash or a `$` the way Go would, and the restore then
// installs a different handler than the one saved.
//
// The order is bash's, which numbers EXIT as 0 and treats DEBUG and ERR
// as pseudo-signals after the real ones. Nothing depends on it, but a
// listing that sorts differently from the shell it is imitating is a
// gratuitous diff in anything comparing the two.
func (r *Runner) printTraps(names []string) {
	// The filter takes the same spellings a spec does: case-insensitive,
	// with or without the SIG prefix, numeric, 0 for EXIT.
	norm := make([]string, 0, len(names))
	for _, n := range names {
		if name, _, ok := lookupSignal(n); ok {
			norm = append(norm, name)
			continue
		}
		u := strings.ToUpper(n)
		if u == "0" {
			u = "EXIT"
		}
		norm = append(norm, u)
	}
	want := func(name string) bool {
		return len(norm) == 0 || slices.Contains(norm, name)
	}
	print := func(name, callback string) {
		r.outf("trap -- %s %s\n", singleQuote(callback), name)
	}
	if r.listed.exit != "" && want("EXIT") {
		print("EXIT", r.listed.exit)
	}
	// Real signals in number order, between EXIT (bash's signal 0) and
	// the pseudo-signals after the real ones. An ignored signal is
	// listed with its empty action: `trap -- '' SIGUSR2` is exactly what
	// restores it.
	for _, s := range signalList() {
		callback, ok := r.sigListed[s.name]
		if !ok || !want(s.name) {
			continue
		}
		r.outf("trap -- %s SIG%s\n", singleQuote(callback), s.name)
	}
	set := []struct{ name, callback string }{
		{"DEBUG", r.listed.debug},
		{"ERR", r.listed.err},
		{"RETURN", r.listed.ret},
	}
	for _, tr := range set {
		if tr.callback == "" {
			continue
		}
		if !want(tr.name) {
			continue
		}
		print(tr.name, tr.callback)
	}
}

// singleQuote wraps s so the shell reads it back byte for byte.
//
// Deliberately not [syntax.Quote], which picks whichever quoting is
// shortest and answers `"echo it's $HOME"` where bash answers
// `'echo it'\”s $HOME'`. Both re-run correctly, so this is about the
// listing being comparable with bash's rather than about safety — but a
// `trap -p` that differs from bash only in its quoting style is a diff in
// every side-by-side, for nothing. Single quotes are safe for every byte
// including newlines; the one exception is a single quote itself, which
// is what the '\” dance is for.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// printSignalNames answers `trap -l`, the numbered signal table.
//
// The numbers are this platform's rather than a fixed list, because they
// differ between Linux and darwin and the number is the part a caller
// acts on — `kill -N` takes it. The set is the portable one, on the same
// rule internal/jobs states for `kill -l`: a name that means different
// things on different unixes is worse than one that is simply absent.
//
// One line per signal, which is koi's `kill -l` rather than bash's five
// columns. In bash the two listings are the same output, and they are
// here too; matching bash's column layout in one of them and not the
// other would make koi disagree with itself, which is the worse of the
// two divergences. TestTrapListMatchesKillList holds them together.
func (r *Runner) printSignalNames() {
	for _, s := range signalList() {
		r.outf("%2d) SIG%s\n", s.num, s.name)
	}
}

// stmtSource renders a statement back to source, which is what
// BASH_COMMAND holds: bash reports `echo $i`, not `echo 1`, because the
// trap runs *before* the expansion.
func stmtSource(st *syntax.Stmt) string {
	var sb strings.Builder
	if err := syntax.NewPrinter(syntax.SingleLine(true)).Print(&sb, st.Cmd); err != nil {
		return ""
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// setSignalTrap arms, ignores, or restores one real signal (#350).
func (r *Runner) setSignalTrap(name string, sig os.Signal, action string, reset bool, setLine uint) {
	if reset {
		signal.Reset(sig)
		delete(r.sigTraps, name)
		delete(r.sigListed, name)
		delete(r.sigTrapLines, name)
		return
	}
	if r.sigTraps == nil {
		r.sigTraps = make(map[string]string)
	}
	if r.sigListed == nil {
		r.sigListed = make(map[string]string)
	}
	if r.sigTrapLines == nil {
		r.sigTrapLines = make(map[string]uint)
	}
	r.sigTraps[name] = action
	r.sigListed[name] = action
	r.sigTrapLines[name] = setLine
	if action == "" {
		// `trap '' SIG` ignores the signal — for this shell, and, since
		// an ignored disposition survives exec, for its children too.
		// Ignore also undoes any earlier Notify for the signal.
		signal.Ignore(sig)
		return
	}
	if r.sigChan == nil {
		r.sigChan = make(chan os.Signal, 32)
		r.sigNames = make(map[os.Signal]string)
	}
	r.sigNames[sig] = name
	signal.Notify(r.sigChan, sig)
}

// runPendingSignalTraps fires the handlers for any real signals that
// arrived since the last statement boundary (#350). That boundary is
// bash's granularity: a signal arriving mid-command runs its trap after
// that command finishes, before the next one starts. A signal arriving
// while nothing is armed for it — the handler was reset after Notify —
// is dropped, which is also what the default disposition would often
// have done by now.
func (r *Runner) runPendingSignalTraps(ctx context.Context) {
	if r.sigChan == nil || r.handlingTrap {
		return
	}
	for {
		select {
		case sig := <-r.sigChan:
			if cb := r.sigTraps[r.sigNames[sig]]; cb != "" {
				name := r.sigNames[sig]
				r.signalTrapCallback(ctx, cb, name, r.sigTrapLines[name])
			}
		default:
			return
		}
	}
}

// trapCallback runs a fake trap's handler. baseLine is what $LINENO
// reports on the action's first line — the triggering command's line for
// DEBUG and ERR, the line the trap was set on for EXIT and RETURN — with
// later action lines counting on from it, which is bash's arithmetic
// (#352). Zero means the action's own positions report as written.
// It reports the action's final exit status, which extdebug's skip rule
// reads off the DEBUG trap (#355); every other caller ignores it.
func (r *Runner) trapCallback(ctx context.Context, callback, name string, baseLine uint) uint8 {
	return r.runTrapCallback(ctx, callback, name, baseLine, false)
}

// signalTrapCallback runs a real signal's handler. It differs from the
// fake traps' in what survives it: `trap 'return' USR1` breaking a busy
// loop out of a function is a documented idiom (bash's own suite uses
// it), so a return, exit, or abort raised inside the handler propagates
// instead of being rolled back with the exit status.
func (r *Runner) signalTrapCallback(ctx context.Context, callback, name string, baseLine uint) {
	_ = r.runTrapCallback(ctx, callback, name, baseLine, true)
}

// runSubshellExitTrap fires this subshell's own EXIT trap: the end of the
// body is that subshell's exit, whether it fell off the end or called
// exit (#353). Callers run it before clearing exit.exiting, and bash's
// exit-overrides-status rule rides on the keep-flow path.
func (r *Runner) runSubshellExitTrap(ctx context.Context) {
	if r.exitTrapFired || r.callbackExit == "" {
		return
	}
	r.exitTrapFired = true
	r.runTrapCallback(ctx, r.callbackExit, "exit", r.callbackExitLine, true)
}

//nolint:unparam // the status feeds extdebug's skip rule via trapCallback
func (r *Runner) runTrapCallback(ctx context.Context, callback, name string, baseLine uint, keepFlow bool) uint8 {
	if callback == "" {
		return 0 // nothing to do
	}
	if r.handlingTrap {
		return 0 // don't recurse, as that could lead to cycles
	}
	r.handlingTrap = true
	defer func() { r.handlingTrap = false }()

	p := syntax.NewParser()
	// TODO: do this parsing when "trap" is called?
	file, err := p.Parse(strings.NewReader(callback), name+" trap")
	if err != nil {
		r.errf(name+"trap: %v\n", err)
		// ignore errors in the callback
		return 0
	}
	oldExit, oldLastExit := r.exit, r.lastExit
	r.lastExit = r.exit
	// The action starts with a clean slate: the exiting or returning
	// already in flight belongs to the shell around the trap, and left in
	// place it either stopped the action's first statement (before
	// handlingTrap suppressed flow in [Runner.stop]) or — with the
	// suppression — kept a `return 2` inside a function the action calls
	// from ending that function, so a trailing `return 0` overwrote the
	// answer extdebug reads (#355). Control flow the action itself raises
	// behaves normally and is inspected below.
	r.exit = exitStatus{}
	// The callback's own statements must not disturb whether the ERR trap has
	// already run for the failure which got us here, as [Runner.stmtSync]
	// clears that for every statement it runs.
	// The same goes for PIPESTATUS: bash leaves it holding the statuses of the
	// pipeline which triggered the trap, not those of the trap's own commands.
	// Note that the variable itself has to be put back, not just the
	// bookkeeping, because the callback's statements will have set it.
	oldErrTrapFired := r.errTrapFired
	oldPipeStatusSet, oldPipeStatus := r.pipeStatusSet, r.pipeStatus
	oldPipeStatusVar := r.lookupVar(shellPipeStatusVar)
	var oldLineOffset uint64
	if r.ecfg != nil {
		oldLineOffset = r.ecfg.LineOffset
		r.ecfg.LineOffset = 0
		if baseLine > 0 {
			r.ecfg.LineOffset = uint64(baseLine) - 1
		}
	}
	r.stmts(ctx, file.Stmts)
	if r.ecfg != nil {
		r.ecfg.LineOffset = oldLineOffset
	}
	r.errTrapFired = oldErrTrapFired
	r.pipeStatusSet, r.pipeStatus = oldPipeStatusSet, oldPipeStatus
	if oldPipeStatusVar.Set {
		r.setVar(shellPipeStatusVar, oldPipeStatusVar)
	}
	code := r.exit.code
	if keepFlow && (r.exit.returning || r.exit.exiting || r.exit.aborting) {
		// A real signal's handler may redirect control — see
		// [Runner.signalTrapCallback] — and rolling that back would trap
		// the shell in the loop the handler exists to break out of.
		return code
	}
	r.exit, r.lastExit = oldExit, oldLastExit // traps on EXIT or ERR should not modify the result
	return code
}

// globalVar reads name from the global scope, skipping every function
// scope in between — what declare -g consults and writes (#379).
func (r *Runner) globalVar(name string) expand.Variable {
	env := expand.Environ(r.writeEnv)
	for {
		o, ok := env.(*overlayEnviron)
		if !ok || !o.funcScope {
			return env.Get(name)
		}
		env = o.parent
	}
}

// parseCompoundArray parses the string form of a compound assignment
// (#379): a declare argument whose value is the text "( ... )". It
// returns nil when the text is not exactly one array literal.
func parseCompoundArray(val string) *syntax.ArrayExpr {
	if !strings.HasPrefix(val, "(") || !strings.HasSuffix(val, ")") {
		return nil
	}
	f, err := syntax.NewParser().Parse(strings.NewReader("_x="+val), "")
	if err != nil || len(f.Stmts) != 1 {
		return nil
	}
	ce, ok := f.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(ce.Assigns) != 1 || len(ce.Args) != 0 {
		return nil
	}
	return ce.Assigns[0].Array
}

// applyArrayKind gives vr the array kind an explicit -a or -A asks for
// (#378): an unset variable keeps its unsetness — `declare -a c` then
// prints bare — a scalar's value carries to element 0, the matching
// kind is a no-op, and the opposite kind is bash's cannot-convert
// error with the data kept and status 1. Measured against 5.3.
func (r *Runner) applyArrayKind(vr *expand.Variable, valType, variant, name string) bool {
	want := expand.Indexed
	other, from, to := expand.Associative, "associative", "indexed"
	if valType == "-A" {
		want = expand.Associative
		other, from, to = expand.Indexed, "indexed", "associative"
	}
	switch vr.Kind {
	case want:
	case other:
		r.errf("%s: %s: cannot convert %s to %s array\n", variant, name, from, to)
		r.exit.code = 1
		return false
	case expand.String:
		if vr.Set {
			if want == expand.Indexed {
				vr.List = []string{vr.Str}
			} else {
				vr.Map = map[string]string{"0": vr.Str}
			}
			vr.Str = ""
		}
		vr.Kind = want
	default:
		vr.Kind = want
	}
	return true
}

func (r *Runner) flattenAssigns(args []*syntax.Assign) iter.Seq[*syntax.Assign] {
	return func(yield func(*syntax.Assign) bool) {
		for _, as := range args {
			// Convert "declare $x" into "declare value".
			// Don't use syntax.Parser here, as we only want the basic
			// splitting by '='.
			if as.Name != nil {
				if !yield(as) {
					return
				}
				continue
			}
			for _, field := range r.fields(as.Value) {
				as := &syntax.Assign{}
				name, val, ok := strings.Cut(field, "=")
				as.Name = &syntax.Lit{Value: name}
				if !ok {
					as.Naked = true
				} else {
					as.Value = &syntax.Word{Parts: []syntax.WordPart{
						&syntax.Lit{Value: val},
					}}
				}
				if !yield(as) {
					return
				}
			}
		}
	}
}

func match(pat, name string) bool {
	matcher, err := shinternal.ExtendedPatternMatcher(pat, pattern.EntireString|pattern.ExtendedOperators)
	if err != nil {
		// An invalid pattern compares as its literal self in bash's
		// case and [[ ]] (#373): the pattern string arrives here with
		// its quoted spans escaped, so unescape before comparing.
		var sb strings.Builder
		for i := 0; i < len(pat); i++ {
			b := pat[i]
			if b == '\\' && i+1 < len(pat) {
				i++
				b = pat[i]
			}
			sb.WriteByte(b)
		}
		return sb.String() == name
	}
	return matcher != nil && matcher(name)
}

func elapsedString(d time.Duration, posix bool) string {
	if posix {
		return fmt.Sprintf("%.2f", d.Seconds())
	}
	mins := int(d.Minutes())
	sec := math.Mod(d.Seconds(), 60.0)
	return fmt.Sprintf("%dm%.3fs", mins, sec)
}

func (r *Runner) stmts(ctx context.Context, stmts []*syntax.Stmt) {
	for _, stmt := range stmts {
		r.stmt(ctx, stmt)
	}
}

// stmtsTopLevel runs a whole file's statements, and is where an `aborting`
// exit stops unwinding and the shell carries on.
//
// bash calls an assignment to a readonly variable fatal, but "fatal" there
// means it throws away the command it is running and goes back to reading
// input -- not that the shell dies. Measured against 5.3, everything after
// the failure *in that command* is skipped and the next one runs:
//
//	readonly foo=one
//	f() { echo in1; foo=4; echo in2; }   # in2 never runs
//	f
//	echo after                           # this does, and the script exits 0
//
// What resumes is the next *line*, not the next statement, because a line is
// bash's reading unit -- the same rule ParseAsRead is built on. On one line,
//
//	readonly foo=one; foo=4; echo done
//
// bash prints the error and `done` never appears. So the statements skipped
// here are the ones that begin no later than the line the aborted statement
// ended on, which covers both shapes with one comparison.
//
// Note this is the non-POSIX behavior, which is the default and the one koi
// was getting wrong. Under `set -o posix` bash really does exit the shell --
// koi cannot express that yet, since it does not accept the option at all
// (#245); the branch belongs with it when it lands.
func (r *Runner) stmtsTopLevel(ctx context.Context, stmts []*syntax.Stmt) {
	for i := 0; i < len(stmts); i++ {
		// Ambient history (#277): recorded before the statement runs,
		// which is bash's order — `history` lists itself, and a
		// `set +o history` is the last line recorded while the
		// `set -o history` that turned recording on never is, because
		// the option was still off when this check ran for it.
		if r.historyHook != nil && r.opts[optHistory] {
			r.historyHook(stmts[i])
		}
		r.stmt(ctx, stmts[i])
		if !r.exit.aborting {
			continue
		}
		r.exit.aborting = false
		resumeAfter := stmts[i].End().Line()
		for i+1 < len(stmts) && stmts[i+1].Pos().Line() <= resumeAfter {
			i++
		}
	}
}

func (r *Runner) hdocReader(rd *syntax.Redirect) (*os.File, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	// We write to the pipe in a new goroutine,
	// as pipe writes may block once the buffer gets full.
	// We still construct and buffer the entire heredoc first,
	// as doing it concurrently would lead to different semantics and be racy.
	//
	// A quoted delimiter means the body never expands, and that includes
	// escape processing — so it does not go through expansion at all (#244).
	if hdoc, ok := literalHdoc(rd); ok {
		go func() {
			pw.WriteString(hdoc)
			pw.Close()
		}()
		return pr, nil
	}
	if rd.Op != syntax.DashHdoc {
		hdoc := r.document(rd.Hdoc)
		go func() {
			pw.WriteString(hdoc)
			pw.Close()
		}()
		return pr, nil
	}
	var buf bytes.Buffer
	var cur []syntax.WordPart
	flushLine := func() {
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(r.document(&syntax.Word{Parts: cur}))
		cur = cur[:0]
	}
	for _, wp := range rd.Hdoc.Parts {
		lit, ok := wp.(*syntax.Lit)
		if !ok {
			cur = append(cur, wp)
			continue
		}
		first := true
		for part := range strings.SplitSeq(lit.Value, "\n") {
			if !first {
				flushLine()
				cur = cur[:0]
			}
			first = false
			part = strings.TrimLeft(part, "\t")
			cur = append(cur, &syntax.Lit{Value: part})
		}
	}
	flushLine()
	go func() {
		pw.Write(buf.Bytes())
		pw.Close()
	}()
	return pr, nil
}

// literalHdoc returns a here-document body that must not be expanded at
// all, and reports whether this is one.
//
// POSIX is unambiguous: "if any character in word is quoted, the
// here-document lines shall not be expanded". Expansion was getting the
// obvious half right — `$HOME`, `$(cmd)`, backquotes and `$((1+1))` all
// stay literal under `<<'X'` — and the escapes wrong, because escapes are
// processed during expansion and by then the delimiter's quoting is gone:
// [expand.Document] runs one backslash pass for both forms, stripping the
// `\` before `\`, `$` and a backquote. That is precisely the *unquoted*
// heredoc's rule, applied to the quoted one.
//
// `cat > file <<'EOF'` is the idiom for writing a file whose content must
// not be interpreted — the quoted delimiter is the whole reason it is
// safe — so the damage was a wrong artifact rather than a refusal. A regex
// spelled `\\d`, a Windows path, a Makefile recipe, a printf format or an
// escaped `\$` landed on disk with a backslash missing, no message, and
// exit 0.
//
// The body is returned as the text it already is rather than re-escaped:
// doubling every backslash would be an exact inverse of the pass in
// expand.go and would become a silent lie the moment that pass changes.
func literalHdoc(rd *syntax.Redirect) (string, bool) {
	if rd.Hdoc == nil || !hdocDelimQuoted(rd.Word) {
		return "", false
	}
	// The body is one literal by the time it gets here: either the lexer
	// built it that way because it agreed the delimiter was quoted, or
	// [relitHeredocs] put it back that way from the source because it had
	// not (#258). Any other shape means neither happened, and treating
	// live expansions as text is the same bug pointing the other way — so
	// leave it to the expanding path.
	//
	// That last case is reachable by one caller: an embedder that parses
	// with [syntax.Parser] itself and hands the tree straight to Run,
	// rather than coming through [ParseAsRead] as every koi entry point
	// does. The repair needs the source text and Run does not have it, so
	// the partially quoted delimiter keeps the parser's answer there. It
	// cannot be fixed from the tree alone: a printed tree is not the
	// source it came from, which is the whole reason relitHeredocs slices
	// instead.
	if len(rd.Hdoc.Parts) != 1 {
		return "", false
	}
	lit, ok := rd.Hdoc.Parts[0].(*syntax.Lit)
	if !ok {
		return "", false
	}
	body := lit.Value
	// `<<-'X'` strips leading tabs from every line. The lexer skips them
	// only when matching the delimiter, not when building the body, so the
	// stripping is the consumer's job either way — see the DashHdoc branch
	// in hdocReader, which does the same thing a line at a time because it
	// has expansions to interleave and this does not.
	if rd.Op != syntax.DashHdoc {
		return body, true
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimLeft(line, "\t")
	}
	return strings.Join(lines, "\n"), true
}

// hdocDelimQuoted reports whether a here-document's delimiter is quoted,
// which is POSIX's test for whether the body expands: "if any character in
// word is quoted, the here-document lines shall not be expanded".
//
// Any character, so any part. This used to ask only the *last* part, to
// match the parser — which decides the same question in unquotedWordBytes
// and overwrites its verdict per part rather than accumulating it, so
// `<<'X'Y` reads there as unquoted. Mirroring that made `<<'X'Y` expand
// its body, from a delimiter whose quote says it must not (#258).
//
// Disagreeing with the parser is safe now, which it was not before:
// [relitHeredocs] has already put such a body back to the literal it was
// written as, so there is a single literal here to hand over rather than
// an expansion tree to mistake for text.
func hdocDelimQuoted(w *syntax.Word) bool { return posixQuotedDelim(w) }

func (r *Runner) redir(ctx context.Context, rd *syntax.Redirect) (io.Closer, error) {
	if rd.Hdoc != nil {
		pr, err := r.hdocReader(rd)
		if err != nil {
			return nil, err
		}
		r.stdin = pr
		return pr, nil
	}

	// Which descriptor this applies to. Input redirections default to 0 and
	// output ones to 1; "{name}>" allocates one and stores its number.
	fd := 1
	switch rd.Op {
	case syntax.RdrIn, syntax.DplIn, syntax.RdrInOut:
		fd = 0
	}
	fdVarName := ""
	if rd.N != nil {
		val := rd.N.Value
		if name, ok := strings.CutPrefix(val, "{"); ok {
			name, ok = strings.CutSuffix(name, "}")
			if !ok || !syntax.ValidName(name) {
				return nil, fmt.Errorf("invalid redirect fd variable: %v", val)
			}
			fdVarName = name
			fd = r.freeFd()
		} else {
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("invalid redirect fd: %v", val)
			}
			fd = n
		}
	}
	orig := &r.stdout
	if fd == 2 {
		orig = &r.stderr
	}
	arg := r.literal(rd.Word)
	switch rd.Op {
	case syntax.WordHdoc:
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		r.stdin = pr
		// We write to the pipe in a new goroutine,
		// as pipe writes may block once the buffer gets full.
		go func() {
			pw.WriteString(arg)
			pw.WriteString("\n")
			pw.Close()
		}()
		return pr, nil
	case syntax.DplOut:
		if arg == "-" {
			r.closeFd(fd)
			return nil, nil
		}
		src, err := strconv.Atoi(arg)
		if err != nil {
			return nil, fmt.Errorf("unhandled %v arg: %q", rd.Op, arg)
		}
		w := r.fdWriter(src)
		if w == nil {
			r.errf("%v: Bad file descriptor\n", src)
			return nil, errBadFd
		}
		r.setFdWriter(fd, w)
		r.setFdVar(fdVarName, fd)
		return nil, nil
	case syntax.RdrIn, syntax.RdrOut, syntax.AppOut,
		syntax.RdrAll, syntax.AppAll, syntax.RdrClob, syntax.RdrInOut:
		// done further below
	case syntax.DplIn:
		if arg == "-" {
			r.closeFd(fd)
			return nil, nil
		}
		src, err := strconv.Atoi(arg)
		if err != nil {
			return nil, fmt.Errorf("unhandled %v arg: %q", rd.Op, arg)
		}
		rwc, ok := r.extraFiles[src]
		if !ok {
			if src != 0 || r.stdin == nil {
				r.errf("%v: Bad file descriptor\n", src)
				return nil, errBadFd
			}
			rwc = r.stdin
		}
		if fd == 0 {
			stdin, err := stdinFile(rwc)
			if err != nil {
				return nil, err
			}
			r.stdin = stdin
		} else {
			r.setFdFile(fd, rwc)
		}
		r.setFdVar(fdVarName, fd)
		return nil, nil
	default:
		return nil, fmt.Errorf("unhandled redirect op: %v", rd.Op)
	}
	// noclobber refuses to truncate an existing regular file. ">|" is the
	// escape hatch which ignores the option, and appending or writing to a file
	// which does not exist yet is always allowed. Note that bash only protects
	// regular files, so ">/dev/null" keeps working.
	if r.opts[optNoClobber] {
		switch rd.Op {
		case syntax.RdrOut, syntax.RdrAll:
			if info, err := r.stat(ctx, arg); err == nil && info.Mode().IsRegular() {
				// Note that the errors which [Runner.redir] returns are not
				// reported by its caller, so we must report this one ourselves.
				err := fmt.Errorf("%s: cannot overwrite existing file", arg)
				r.errf("%v\n", err)
				return nil, err
			}
		}
	}
	mode := os.O_RDONLY
	switch rd.Op {
	case syntax.AppOut, syntax.AppAll:
		mode = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	case syntax.RdrOut, syntax.RdrAll, syntax.RdrClob:
		mode = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	case syntax.RdrInOut:
		// "<>" opens for reading and writing without truncating.
		mode = os.O_RDWR | os.O_CREATE
	}
	f, err := r.open(ctx, arg, mode, 0o644, true)
	if err != nil {
		return nil, err
	}
	switch rd.Op {
	case syntax.RdrIn, syntax.RdrInOut:
		if fd == 0 {
			stdin, err := stdinFile(f)
			if err != nil {
				return nil, err
			}
			r.stdin = stdin
		} else {
			r.setFdFile(fd, f)
		}
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob:
		if fd <= 2 {
			*orig = f
		} else {
			r.setFdFile(fd, f)
		}
	case syntax.RdrAll, syntax.AppAll:
		r.stdout = f
		r.stderr = f
	default:
		return nil, fmt.Errorf("unhandled redirect op: %v", rd.Op)
	}
	r.setFdVar(fdVarName, fd)
	return f, nil
}

// setFdVar records the descriptor a "{name}>" redirection allocated.
func (r *Runner) setFdVar(name string, fd int) {
	if name == "" {
		return
	}
	r.setVarString(name, strconv.Itoa(fd))
}

// errBadFd is returned when a redirection names a descriptor which is not open.
// It is already reported when it is returned, unlike the other errors here.
var errBadFd = errors.New("bad file descriptor")

func (r *Runner) loopStmtsBroken(ctx context.Context, stmts []*syntax.Stmt) bool {
	oldInLoop := r.inLoop
	r.inLoop = true
	defer func() { r.inLoop = oldInLoop }()
	for _, stmt := range stmts {
		r.stmt(ctx, stmt)
		if r.contnEnclosing > 0 {
			r.contnEnclosing--
			return r.contnEnclosing > 0
		}
		if r.breakEnclosing > 0 {
			r.breakEnclosing--
			return true
		}
	}
	return false
}

func (r *Runner) call(ctx context.Context, pos syntax.Pos, args []string) {
	if r.stop(ctx) {
		return
	}
	if r.callHandler != nil {
		var err error
		args, err = r.callHandler(r.handlerCtx(ctx, handlerKindCall, pos), args)
		if err != nil {
			// handler's custom fatal error
			r.exit.fatal(err)
			return
		}
	}
	name := args[0]
	if body := r.Funcs[name]; body != nil {
		// FUNCNEST bounds how deep function calls may nest (#349). The
		// violation is the readonly-assignment shape: the whole function
		// stack unwinds — not even a `||` on a caller's line sees the
		// status — and the top level carries on with status 1, so a
		// script file continues at its next line while -c, one input
		// unit, loses its remainder and exits 1. Only a wholly numeric,
		// positive value binds; bash ignores FUNCNEST=0, negatives, and
		// anything non-numeric.
		if limit, err := strconv.Atoi(r.envGet("FUNCNEST")); err == nil && limit > 0 {
			depth := 0
			for _, frame := range r.frames {
				if frame.isFunc {
					depth++
				}
			}
			if depth >= limit {
				r.errf("%s: maximum function nesting level exceeded (%d)\n", name, limit)
				r.exit.code = 1
				r.exit.aborting = true
				return
			}
		}
		// stack them to support nested func calls
		oldParams := r.Params
		r.Params = args[1:]
		oldInFunc := r.inFunc
		r.inFunc = true

		// A function is its own level for the ERR trap: bash runs it inside the
		// function only with -E, and runs it again for the call either way.
		oldErrTrapFired, oldErrTrapDepth := r.errTrapFired, r.errTrapDepth
		r.errTrapFired, r.errTrapDepth = false, r.errTrapDepth+1

		// The call is one command as far as this level is concerned, so it gets
		// its own PIPESTATUS whatever the body did.
		oldPipeStatusSet, oldPipeStatus := r.pipeStatusSet, r.pipeStatus
		r.pipeStatusSet, r.pipeStatus = false, nil

		// BASH_SOURCE names where the function was *defined*, so a helper
		// in a sourced library reports the library rather than whichever
		// script happened to call it. The line is the call's, in whatever
		// file the call was written in.
		popFrame := r.pushFrame(callFrame{
			name:     name,
			source:   r.funcSource[name],
			callLine: pos.Line(),
			isFunc:   true,
		})

		oldReturnTrapOff := r.enterFuncForReturnTrap()

		// Functions run in a nested scope.
		// Note that [Runner.exec] below does something similar.
		origEnv := r.writeEnv
		r.writeEnv = &overlayEnviron{parent: r.writeEnv, funcScope: true}

		r.stmt(ctx, body)

		// Before the scope and the frame go: the trap sees the
		// function's locals and its FUNCNAME, as bash's does.
		r.runReturnTrap(ctx)
		r.returnTrapOff = oldReturnTrapOff

		// The same rule for EXIT when `exit` was called in here (#352):
		// bash fires the EXIT trap where the exit happened, so the
		// action sees this function's FUNCNAME rather than an empty
		// stack. Run's own firing point skips it once fired.
		if r.exit.exiting && !r.exitTrapFired && !r.handlingTrap && r.callbackExit != "" {
			r.exitTrapFired = true
			r.runTrapCallback(ctx, r.callbackExit, "exit", r.callbackExitLine, true)
		}

		r.writeEnv = origEnv

		r.errTrapFired, r.errTrapDepth = oldErrTrapFired, oldErrTrapDepth
		r.pipeStatusSet, r.pipeStatus = oldPipeStatusSet, oldPipeStatus
		popFrame()
		r.Params = oldParams
		r.inFunc = oldInFunc
		r.exit.returning = false
		return
	}
	if IsBuiltin(name) {
		r.exit = r.builtin(ctx, pos, name, args[1:])
		return
	}
	r.exec(ctx, pos, args)
}

func (r *Runner) exec(ctx context.Context, pos syntax.Pos, args []string) {
	r.execWith(ctx, pos, "", false, args)
}

// execWith is [Runner.exec] with the two things "exec"'s own flags can change
// about how the program runs: an argv[0] which differs from the file being run
// ("exec -a name file", and "exec -l"), and an empty environment ("exec -c").
// An empty argv0 and a false clearEnv mean neither applies, which is every
// caller other than the exec builtin.
func (r *Runner) execWith(ctx context.Context, pos syntax.Pos, argv0 string, clearEnv bool, args []string) {
	hctx := r.handlerCtx(ctx, handlerKindExec, pos)
	if argv0 != "" || clearEnv {
		hc := HandlerCtx(hctx)
		hc.Argv0 = argv0
		hc.ClearEnv = clearEnv
		hctx = context.WithValue(hctx, handlerCtxKey{}, hc)
	}
	r.exit.fromHandlerError(r.execHandler(hctx, args))
}

func (r *Runner) open(ctx context.Context, path string, flags int, mode os.FileMode, print bool) (io.ReadWriteCloser, error) {
	// If we are opening a FIFO temporary file created by the interpreter itself,
	// don't pass this along to the open handler as it will not work at all
	// unless [os.OpenFile] is used directly with it.
	// Matching by directory and basename prefix isn't perfect, but works.
	//
	// If we want FIFOs to use a handler in the future, they probably
	// need their own separate handler API matching Unix-like semantics.
	dir, name := filepath.Split(path)
	dir = strings.TrimSuffix(dir, "/")
	if dir == r.tempDir && strings.HasPrefix(name, fifoNamePrefix) {
		return os.OpenFile(path, flags, mode)
	}

	f, err := r.openHandler(r.handlerCtx(ctx, handlerKindOpen, todoPos), path, flags, mode)
	// TODO: support wrapped PathError returned from openHandler.
	switch err.(type) {
	case nil:
		return f, nil
	case *os.PathError:
		if print {
			r.errf("%v\n", err)
		}
	default: // handler's custom fatal error
		r.exit.fatal(err)
	}
	return nil, err
}

func (r *Runner) stat(ctx context.Context, name string) (fs.FileInfo, error) {
	path := absPath(r.Dir, name)
	return r.statHandler(r.handlerCtx(ctx, handlerKindStat, todoPos), path, true)
}

func (r *Runner) lstat(ctx context.Context, name string) (fs.FileInfo, error) {
	path := absPath(r.Dir, name)
	return r.statHandler(r.handlerCtx(ctx, handlerKindStat, todoPos), path, false)
}

func (r *Runner) access(ctx context.Context, name string, mode AccessMode) error {
	path := absPath(r.Dir, name)
	return r.accessHandler(r.handlerCtx(ctx, handlerKindAccess, todoPos), path, mode)
}
