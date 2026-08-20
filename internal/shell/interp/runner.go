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
			if cs.TempFile || cs.ReplyVar {
				// bash 5.3's funsubs exist to run in the *current*
				// shell (#421): `x=${ v=inside; }` is how a script gets
				// a value out of a command *and* keeps what it did.
				// koi ran them in a subshell like an ordinary
				// substitution, so nothing they changed survived.
				return r.funSubst(ctx, w, cs)
			}
			r2 := r.subshell(false)
			// A command substitution sees the caller's jobs. bash
			// draws the line here rather than at "is this a
			// subshell": `echo $(jobs -r | wc -l)` counts them and
			// `( jobs -r | wc -l )` does not, and it is the former
			// that every bounded parallel loop is built out of
			// (#302). They arrive non-waitable; see bgProc.inherited.
			r2.bgProcs = inheritedJobs(r.bgProcs)
			// A command substitution runs *without* errexit unless
			// inherit_errexit asks for it (#412). koi passed it down,
			// so `echo $(false; echo ok)` under `set -e` printed
			// nothing where bash prints ok — the body stopped at the
			// false and the caller carried on with an empty value.
			if !r.opts[optInheritErrExit] {
				r2.opts[optErrExit] = false
			}
			r2.stdout = w
			r2.stmts(ctx, cs.Stmts)
			r2.runSubshellExitTrap(ctx)
			r2.exit.exiting = false  // subshells don't exit the parent shell
			r2.exit.aborting = false // nor unwind it: an abort inside a subshell ends that subshell
			// Nor does a `return` inside one return from the enclosing
			// function: it ends the subshell with that status, and the
			// caller carries on (#422).
			r2.exit.returning = false
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
				r2.exit.exiting = false   // subshells don't exit the parent shell
				r2.exit.aborting = false  // nor unwind it: an abort inside a subshell ends that subshell
				r2.exit.returning = false // nor return from the enclosing function (#422)
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
	// Word expansion happens *before* a command's own redirections, so
	// the message goes to the stderr that was in force before them
	// (#469). koi applies redirections first, so `echo $nope
	// 2>/dev/null` under `set -u` sent the diagnostic into /dev/null
	// and the script stopped mid-unit with nothing said.
	stderr := r.stderr
	if r.preRedirStderr != nil {
		stderr = r.preRedirStderr
	}
	fmt.Fprintln(stderr, errMsg)
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
	env := &overlayEnviron{parent: r.writeEnv}
	// An exported function travels to a child as bash spells it: an
	// environment entry named BASH_FUNC_<name>%% holding the definition
	// (#387). It is laid into the handler's overlay rather than the
	// shell's own scope, so it reaches execEnv without appearing in any
	// listing the session itself makes.
	for name := range r.exportedFuncs {
		body := r.Funcs[name]
		if body == nil {
			continue
		}
		var buf bytes.Buffer
		syntax.NewPrinter().Print(&buf, body)            //nolint:errcheck // writing to a buffer
		env.Set("BASH_FUNC_"+name+"%%", expand.Variable{ //nolint:errcheck // the overlay never errors here
			Set: true, Exported: true, Kind: expand.String,
			Str: "() " + buf.String(),
		})
	}
	hc := HandlerContext{
		runner:         r,
		kind:           kind,
		Env:            env,
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
// setInputFd points a descriptor at an open file for reading. fd 0 is
// the shell's own stdin and goes through stdinFile, which is what the
// rest of the interpreter reads; any other descriptor is an ordinary
// entry in the table.
func (r *Runner) setInputFd(fd int, f *os.File) error {
	if fd == 0 {
		stdin, err := stdinFile(f)
		if err != nil {
			return err
		}
		r.stdin = stdin
		return nil
	}
	r.setFdFile(fd, f)
	return nil
}

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
			// Nor does a `return` inside one return from the enclosing
			// function: it ends the subshell with that status, and the
			// caller carries on (#422).
			r2.exit.returning = false
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

	r.traceLine = st.Pos().Line()

	oldIn, oldOut, oldErr := r.stdin, r.stdout, r.stderr
	// An expansion error is reported to the stderr this statement had
	// before its own redirections (#469); an enclosing block's
	// redirection is still in force, which is why this is per statement
	// rather than the runner's original stderr.
	oldPreRedir := r.preRedirStderr
	r.preRedirStderr = oldErr
	defer func() { r.preRedirStderr = oldPreRedir }()
	// The descriptor table is modified in place, so a statement with its own
	// redirections gets a copy to modify and the original is put back after.
	oldExtraFiles := r.extraFiles
	if len(st.Redirs) > 0 {
		r.extraFiles = maps.Clone(r.extraFiles)
	}
	var closers []io.Closer
	r.varRedirFds = nil
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
		if st.Negated {
			// A negated statement is immune to errexit, and the
			// suppression has to be in force *while it runs*: koi
			// applied the negation afterwards, so `! eval false` under
			// set -e took the shell down inside eval before the `!`
			// was ever considered (#412), truncating the rest of the
			// script.
			oldNoErrExit := r.noErrExit
			r.noErrExit = true
			r.cmd(ctx, st.Cmd)
			r.noErrExit = oldNoErrExit
		} else {
			r.cmd(ctx, st.Cmd)
		}
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
		// A descriptor a {varname} redirection allocated outlives the
		// command that opened it — that is the whole point of the form,
		// and bash only closes it under "varredir_close" (#418). koi
		// undid it with every other redirection, so `: {fd}>&1` left
		// $fd naming a descriptor that was already gone.
		var keep map[int]io.ReadWriteCloser
		for _, fd := range r.varRedirFds {
			if r.opts[optVarRedirClose] {
				break // the option asks for them to be closed after all
			}
			if f, ok := r.extraFiles[fd]; ok {
				if keep == nil {
					keep = make(map[int]io.ReadWriteCloser, len(r.varRedirFds))
				}
				keep[fd] = f
			}
		}
		r.extraFiles = oldExtraFiles
		if len(keep) > 0 {
			// Cloned rather than written through: oldExtraFiles is the
			// enclosing scope's map, not this statement's copy.
			r.extraFiles = maps.Clone(r.extraFiles)
			if r.extraFiles == nil {
				r.extraFiles = make(map[int]io.ReadWriteCloser, len(keep))
			}
			maps.Copy(r.extraFiles, keep)
		}
		r.varRedirFds = nil
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
		r2.exit.exiting = false   // subshells don't exit the parent shell
		r2.exit.aborting = false  // nor unwind it: an abort inside a subshell ends that subshell
		r2.exit.returning = false // nor return from the enclosing function (#422)
		r.exit = r2.exit
	case *syntax.CallExpr:
		// An alias is replacement *text*, spliced into the command line
		// and re-parsed (#407): that is what lets one hold a
		// redirection, a `;`, a newline, or another alias. Building a
		// word list out of it at definition time refused most real
		// aliases outright.
		if r.opts[optExpandAliases] && len(cm.Args) > 0 {
			if stmts, name, ok := r.expandAlias(cm); ok {
				// The guard has to hold while the spliced command
				// *runs*, not while it is parsed: `alias ls='ls -d'`
				// re-expands its own head otherwise, forever.
				if r.expandingAlias == nil {
					r.expandingAlias = map[string]bool{}
				}
				r.expandingAlias[name] = true
				r.stmts(ctx, stmts)
				delete(r.expandingAlias, name)
				return
			}
		}
		// Build new slices, to not modify the caller's AST
		// nor the slices in the alias map.
		args := cm.Args
		assigns := cm.Assigns
		if r.opts[optKeyword] && len(args) > 1 {
			// `set -k` puts *every* assignment-shaped word into the
			// command's environment, not just the ones before the
			// command name (#396): `echo hi c=7` runs `echo hi` with c
			// bound. koi refused the option outright, so varenv.tests
			// stopped at the refusal.
			//
			// The split has to happen on the *word*, before expansion:
			// bash decides from what was written, so `echo "x=1"` is an
			// argument and a value that merely expands to something
			// containing `=` is too. Splitting expanded fields ate
			// `echo "after=[$c]"`.
			var kw []*syntax.Assign
			args, kw = splitKeywordAssigns(args)
			if len(kw) > 0 {
				assigns = append(slices.Clone(cm.Assigns), kw...)
			}
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
				if prev.Kind == expand.NameRef && as.Index == nil {
					// The reference may name an array *element* (#389):
					// `declare -n b="a[1]"; b=v` writes that element,
					// where following the name alone wrote a variable
					// literally called "a[1]" and lost the assignment.
					if base, sub, ok := r.nameRefElem(prev.Str); ok {
						name, as = base, withIndex(as, sub)
						prev = r.lookupVar(base)
					}
				}
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
					if as.Append {
						// An append is traced as the append, not as
						// its result: koi printed `foo=onetwo` where
						// bash prints `foo+=two`, which reads as a
						// different assignment (#413).
						appended, qerr := syntax.Quote(r.literal(as.Value), syntax.LangBash)
						if qerr == nil {
							val = appended
						}
						trace.stringf("%s+=%s", name, val)
					} else {
						trace.stringf("%s=%s", name, val)
					}
				}
				trace.newLineFlush()
			}
			// An assignment-only command clears `$_` rather than
			// leaving the previous command's last argument (#408).
			r.setVarString("_", "")
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

		// bash treats declaration utilities specially (#380): the temp
		// binding is what the utility sees, and when the utility
		// *declares* the name — not merely queries it with -p/-f/-F —
		// the binding is promoted rather than unwound. declClause
		// records what it declared into declTempNames.
		isDeclUtility := false
		switch fields[0] {
		case "declare", "typeset", "local", "export", "readonly", "nameref":
			isDeclUtility = len(assigns) > 0
		}
		var prevDeclTemp, prevDeclBound map[string]bool
		if isDeclUtility {
			prevDeclTemp, prevDeclBound = r.declTempNames, r.declTempBound
			r.declTempNames = map[string]bool{}
			r.declTempBound = make(map[string]bool, len(assigns))
			for _, as := range assigns {
				r.declTempBound[as.Name.Value] = true
			}
		}

		for _, as := range assigns {
			name := as.Name.Value
			prev := r.lookupVar(name)
			// Resolve any nameref so we can restore the original final value later on.
			if n, v := prev.Resolve(r.writeEnv); n != "" {
				name, prev = n, v
			}

			name, vr := r.assignVal(name, prev, as, "")
			// Inline command vars are always exported.
			vr.Exported = true
			// Like a plain assignment, the temp binding modifies the
			// dynamically scoped variable rather than creating a local
			// of the current function (#380): that is the scope a
			// declaration utility promotes it in, and the unwind below
			// writes the same way so the two stay symmetric.
			vr.Local = false

			restores = append(restores, restoreVar{name, prev})

			r.setVar(name, vr)
		}

		trace.call(fields[0], fields[1:]...)
		trace.newLineFlush()

		if r.traceHook == nil {
			r.call(ctx, cm.Args[0].Pos(), fields)
		} else {
			r.tracedCall(ctx, cm, fields)
		}
		// `$_` is the last argument of the command that just ran, or
		// the command's own name when it had none (#408). It was the
		// shell's path forever, which flips every `${_+word}` probe.
		last := fields[0]
		if len(fields) > 1 {
			last = fields[len(fields)-1]
		}
		r.setVarString("_", last)
		declared := r.declTempNames
		if isDeclUtility {
			r.declTempNames, r.declTempBound = prevDeclTemp, prevDeclBound
		}
		for _, restore := range restores {
			if restore.vr.ReadOnly {
				// The assignment failed and was already reported, so there is
				// nothing to put back and trying would report it a second time.
				continue
			}
			// The unwind writes the same dynamic scope the temp binding
			// landed in; carrying Local through would create a local of
			// the current function instead (the overlay re-derives
			// Local from the entry it lands on).
			restore.vr.Local = false
			if declaredLocal, ok := declared[restore.name]; ok {
				// The declaration utility declared this name (#380),
				// measured against bash 5.3. Non-locally — export,
				// readonly, top-level declare — the temp binding is
				// promoted: `foo="" export foo` keeps foo, so nothing
				// is unwound. As a function-local, the new local
				// shadows the scope the temp binding landed in, so the
				// unwind writes one scope below it — restoring through
				// setVar would clobber the local the declaration just
				// made, which is exactly the inversion this fixes.
				if !declaredLocal {
					continue
				}
				if o, ok := r.writeEnv.(*overlayEnviron); ok && o.funcScope {
					if w, ok := o.parent.(expand.WriteEnviron); ok {
						w.Set(restore.name, restore.vr) //nolint:errcheck // best-effort unwind, like setVar
						continue
					}
				}
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
				r2.exit.exiting = false   // subshells don't exit the parent shell
				r2.exit.aborting = false  // nor unwind it: an abort inside a subshell ends that subshell
				r2.exit.returning = false // nor return from the enclosing function (#422)
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
			if !syntax.ValidName(name) {
				// Assigning to `1` is nonsense bash refuses up front,
				// where koi ran the loop and quietly shadowed the
				// positional parameter (#409).
				r.errf("`%s': not a valid identifier\n", name)
				r.exit.code = 1
				return
			}
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
						if !r.setLoopVar(name, items[c-1]) {
							break
						}
					} else if !r.setLoopVar(name, "") {
						break
					}

					// execute commands until break or return is encountered
					if r.loopStmtsBroken(ctx, cm.Do) {
						break
					}
				}
				break
			}

			for _, field := range items {
				if !r.setLoopVar(name, field) {
					break
				}
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
			// Each evaluation in the header is traced, which is how a
			// trace of a counting loop reads as a loop at all: koi
			// traced only the body, so three iterations looked like
			// three unrelated commands (#413).
			traceArithm := func(x syntax.ArithmExpr) {
				t := r.tracer()
				if t == nil {
					return
				}
				// The compact printer #386 built for `declare -f` is
				// what renders this: syntax.Printer takes commands
				// rather than bare expressions, and printing an
				// ArithmCmd wraps the header in line breaks and
				// re-spaces it — `(( i = 0 ))` where bash echoes what
				// was written.
				t.stringf("(( %s ))", printArithm(x))
				t.newLineFlush()
			}
			if y.Init != nil {
				traceArithm(y.Init)
				r.arithm(y.Init)
			}
			// A failing body command does not end the loop (#369):
			// `for ((f=0; f<3; f++)); do …; false; done` runs all three
			// iterations in bash, and ((i++)) from zero answers status 1
			// on its first step, which used to stop a loop whose update
			// lived in its body. Only control flow ends it early — and
			// an arithmetic error in the update, which raises exactly
			// that.
			for {
				if y.Cond != nil {
					traceArithm(y.Cond)
					if r.arithm(y.Cond) == 0 {
						break
					}
				}
				if r.loopStmtsBroken(ctx, cm.Do) {
					break
				}
				if y.Post != nil {
					traceArithm(y.Post)
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
		r.testCallName = "[["
		defer func() { r.testCallName = "" }()
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
	unref := false   // "+n": detach a nameref
	inherit := false // "-I": take the enclosing scope's value and attributes
	switch variant {
	case "declare", "typeset":
		// When used in a function, "declare" acts as "local"
		// unless the "-g" option is used. typeset is its synonym and
		// was missing here, so a typeset inside a function wrote the
		// global and leaked (#382) — `typeset IFS=:` poisoned every
		// later expansion in a file.
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
		if as.Naked && as.Name.Value == "--" {
			// The end-of-options marker is not a variable name: bare
			// `declare --` lists like bare `declare` (#384), and
			// `declare -- v=1` declares v.
			continue assignLoop
		}
		fp := flagParser{remaining: []string{as.Name.Value}}
		// Note that this consumes every flag clustered into the one
		// argument before moving on; "declare -ri" is -r and -i, and
		// stopping after the first silently dropped the rest.
		sawFlag := fp.more()
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-x", "-r", "-i", "+i", "-t", "+t", "+x":
				modes = append(modes, flag)
			case "+r":
				// bash refuses to drop readonly — a variable cannot be
				// made writable again — and says so per name below.
				modes = append(modes, flag)
			case "-u", "-l", "-c", "+u", "+l", "+c":
				// Two case modifications cancel rather than stack:
				// `declare -ul x` leaves neither, measured (#385).
				if i := slices.IndexFunc(modes, isCaseMode); i >= 0 {
					if modes[i][1] != flag[1] {
						modes = slices.Delete(modes, i, i+1)
						break
					}
					modes = slices.Delete(modes, i, i+1)
				}
				modes = append(modes, flag)
			case "-I":
				inherit = true
			case "-":
				// `local -` saves the shell options and restores them
				// when the function returns.
				if variant != "local" && !r.inFunc {
					r.errf("%s: invalid option %q\n", variant, flag)
					r.exit.code = 2
					return
				}
				r.saveLocalOpts()
				continue assignLoop
			case "-n":
				// -n means two different things: a nameref for
				// declare/local/typeset, and "remove the export
				// attribute" for export, which is bash's (#387).
				if variant == "export" {
					modes = append(modes, "+x")
					break
				}
				valType = flag
			case "-a", "-A":
				valType = flag
			case "+n":
				unref = true
			case "-g":
				global = true
			case "-f", "-p", "-F":
				// -p alongside -f is a modifier, not a replacement:
				// `declare -f -p name` prints the function, where
				// letting -p win looked for a *variable* by that name
				// and answered "not found" (#386).
				if flag == "-p" && (declQuery == "-f" || declQuery == "-F") {
					break
				}
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
		// A function name is not a variable name: `foo-bar(){ :; }`
		// defines and runs, so `export -f foo-bar` must not refuse it
		// (#387). Only the variable paths validate.
		if declQuery != "-f" && declQuery != "-F" && !syntax.ValidName(name) {
			r.errf("%s: invalid name %q\n", variant, name)
			r.exit.code = 1
			return
		}
		if declQuery == "-f" || declQuery == "-F" {
			// `export -f name` and `declare -xf name` export the
			// function rather than printing it (#387); koi printed the
			// body and left the child with a 127.
			// +x is tested first: `export -nf f` carries both, since
			// the export variant contributes -x of its own.
			if slices.Contains(modes, "+x") {
				delete(r.exportedFuncs, name)
				continue
			}
			if slices.Contains(modes, "-x") {
				if r.Funcs[name] == nil {
					r.errf("%s: %s: not a function\n", variant, name)
					r.exit.code = 1
					continue
				}
				if r.exportedFuncs == nil {
					r.exportedFuncs = map[string]bool{}
				}
				r.exportedFuncs[name] = true
				continue
			}
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
			r.printDeclared(name, vr)
			continue
		}
		if unref {
			r.unsetNameRef(name, as)
			continue
		}
		if valType == "-n" && as.Value != nil {
			// A nameref's target must be a name, optionally with a
			// subscript, and may not be the reference itself (#389).
			// Both were accepted silently, so `declare -n foo=12345`
			// made a reference to nothing and every later read of foo
			// answered empty rather than saying why.
			target := r.literalAssign(as.Value)
			if !validNameRefTarget(target) {
				if target == "" {
					r.errf("%s: `%s': not a valid identifier\n", variant, target)
				} else {
					r.errf("%s: `%s': invalid variable name for name reference\n", variant, target)
				}
				r.exit.code = 1
				continue
			}
			if target == name {
				r.errf("%s: %s: nameref variable self references not allowed\n", variant, name)
				r.exit.code = 1
				continue
			}
		}
		vr := r.lookupVar(name)
		freshLocal := false
		if local && !global && !r.localInScope(name) && !r.declTempBound[name] && !inherit {
			freshLocal = true
			// A declaration that creates a *new* local starts from an
			// unset variable rather than inheriting the outer one
			// (#381): `V=abc; f(){ local V; echo "${V-unset}"; }`
			// answers unset, and inheritance is only bash's
			// localvar_inherit. Attributes do not carry either, with
			// one measured exception — an exported outer variable
			// keeps its local shadow exported — while a readonly one
			// refuses the declaration and leaves the outer in place.
			if vr.ReadOnly {
				r.errf("%s: %s: readonly variable\n", variant, name)
				r.exit.code = 1
				continue
			}
			vr = expand.Variable{Exported: vr.Exported}
			if r.opts[optLocalVarInherit] {
				vr = r.lookupVar(name)
				vr.Local = false
				freshLocal = false
			}
		}
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
			case "":
				if freshLocal {
					// The reset above is the value: a new local is
					// declared-but-unset, so KeepValue below — which
					// asks the store to keep the outer variable's
					// value — is exactly what must not happen (#381).
					break
				}
				vr.Kind = expand.KeepValue
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
			case "+x":
				vr.Exported = false
			case "-r":
				vr.ReadOnly = true
			case "+r":
				// A readonly variable can never be made writable
				// again; bash reports it and carries on (#385).
				if vr.ReadOnly {
					r.errf("%s: %s: readonly variable\n", variant, name)
					r.exit.code = 1
				}
			case "-i":
				vr.Integer = true
			case "+i":
				vr.Integer = false
			case "-t":
				vr.Trace = true
			case "+t":
				vr.Trace = false
			case "-u", "-l", "-c":
				vr.CaseMod = mode[1]
			case "+u", "+l", "+c":
				vr.CaseMod = 0
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
		if r.declTempNames != nil {
			// A temp-env prefix assignment is in flight (#380); tell
			// the call site this name was declared, and whether this
			// declaration created a function-local in the current
			// scope — export/readonly inherit an outer local without
			// creating one, and there the temp binding is promoted in
			// place rather than unwound below a new shadow.
			r.declTempNames[name] = local && !global
		}
	}
	if !namedAny && declQuery != "-f" && declQuery != "-F" && !unref && variant != "local" {
		// An attribute flag with no operands lists the variables that
		// carry it, and a bare `declare -p` lists them all (#384) —
		// `declare -A` after `declare -A f` prints f. Bare `declare`
		// and `declare --` instead print POSIX name=value pairs, which
		// is bash's other listing shape.
		match := func(vr expand.Variable) bool { return true }
		switch {
		case valType == "-n":
			match = func(vr expand.Variable) bool { return vr.Kind == expand.NameRef }
		case valType == "-a":
			match = func(vr expand.Variable) bool { return vr.Kind == expand.Indexed }
		case valType == "-A":
			match = func(vr expand.Variable) bool { return vr.Kind == expand.Associative }
		case slices.Contains(modes, "-x"):
			match = func(vr expand.Variable) bool { return vr.Exported }
		case slices.Contains(modes, "-r"):
			match = func(vr expand.Variable) bool { return vr.ReadOnly }
		case slices.Contains(modes, "-i"):
			match = func(vr expand.Variable) bool { return vr.Integer }
		case declQuery != "-p":
			// Bare `declare`, with no attribute to filter on.
			match = nil
		}
		var names []string
		r.writeEnv.Each(func(name string, vr expand.Variable) bool {
			if vr.Declared() && (match == nil || match(vr)) {
				names = append(names, name)
			}
			return true
		})
		slices.Sort(names)
		for _, name := range names {
			vr := r.lookupVar(name)
			if match == nil {
				r.outf("%s=%s\n", name, vr.String())
				continue
			}
			r.printDeclared(name, vr)
		}
	}
	if !namedAny && (declQuery == "-f" || declQuery == "-F") {
		// A bare "declare -f" or "declare -F" lists every function, sorted
		// by name as bash does. Claude Code's shell snapshot uses the
		// latter to carry the user's functions into an agent subshell.
		// With -x the listing is filtered to the exported functions,
		// where koi listed every one (#388) — which is why its
		// func.tests output carried two full dumps of every function
		// defined in the file.
		onlyExported := slices.Contains(modes, "-x")
		names := make([]string, 0, len(r.Funcs))
		for name := range r.Funcs {
			if onlyExported && !r.exportedFuncs[name] {
				continue
			}
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if declQuery == "-f" {
				r.printFuncDef(name, r.Funcs[name])
			}
			switch {
			case onlyExported:
				// bash reports an exported function as `declare -fx`,
				// after the body when the body was asked for.
				r.outf("declare -fx %s\n", name)
			case declQuery == "-F":
				r.outf("declare -f %s\n", name)
			}
		}
	}
}

// printFuncDef prints a function's definition, as "declare -f" does. Note that
// the layout differs from bash's, which indents the body over several lines.
func (r *Runner) printFuncDef(name string, body *syntax.Stmt) {
	r.out(printFuncCanonical(name, body, false))
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
		if !ok && r.sigIgnoredAtEntry[s.name] {
			// Inherited ignores are part of the listing: a script
			// saving and restoring traps has to know about them.
			callback, ok = "", true
		}
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
	// The whole statement, redirections included (#445). Printing only
	// st.Cmd dropped them, so `echo one > /dev/null` reached a DEBUG or
	// ERR trap as `echo one` — a different string from bash's, and the
	// text is what a debugger or logger matching on BASH_COMMAND acts on.
	//
	// bash re-renders rather than quoting the source, and the shape was
	// measured rather than guessed. It normalizes spacing: `echo a
	// >/dev/null` and `echo b   >   /dev/null` both answer
	// `> /dev/null`. A space follows the operator when the target is a
	// word, and does not when the redirection is a dup or a close
	// (`2>&1`, `4>&-`) or a heredoc (`<<EOF`) — which is why this walks
	// the redirects instead of handing the statement to the printer,
	// whose own style writes `>/dev/null` throughout.
	//
	// A heredoc keeps its body and terminator, with real newlines: bash
	// answers "cat <<EOF > /dev/null\nbody line\nEOF\n" byte for byte,
	// where the obvious guess is that it prints the operator alone.
	printer := syntax.NewPrinter(syntax.SingleLine(true))
	print := func(sb *strings.Builder, node syntax.Node) bool {
		if err := printer.Print(sb, node); err != nil {
			return false
		}
		return true
	}

	var sb strings.Builder
	if !print(&sb, st.Cmd) {
		return ""
	}
	line := strings.TrimSuffix(sb.String(), "\n")

	var bodies strings.Builder
	for _, rd := range st.Redirs {
		var part strings.Builder
		if rd.N != nil {
			if !print(&part, rd.N) {
				return ""
			}
		}
		op := rd.Op.String()
		part.WriteString(op)
		switch rd.Op {
		case syntax.DplIn, syntax.DplOut, syntax.Hdoc, syntax.DashHdoc:
			// Tight: a dup names a descriptor, and a heredoc's word is
			// its delimiter rather than a target.
		default:
			part.WriteString(" ")
		}
		if rd.Word != nil {
			if !print(&part, rd.Word) {
				return ""
			}
		}
		line += " " + strings.TrimSuffix(part.String(), "\n")
		if rd.Hdoc != nil {
			var body strings.Builder
			if !print(&body, rd.Hdoc) {
				return ""
			}
			bodies.WriteString("\n")
			bodies.WriteString(strings.TrimSuffix(body.String(), "\n"))
			bodies.WriteString("\n")
			var delim strings.Builder
			if rd.Word != nil && print(&delim, rd.Word) {
				bodies.WriteString(strings.TrimSuffix(delim.String(), "\n"))
			}
			bodies.WriteString("\n")
		}
	}
	return line + bodies.String()
}

// setSignalTrap arms, ignores, or restores one real signal (#350).
func (r *Runner) setSignalTrap(name string, sig os.Signal, action string, reset bool, setLine uint) {
	if r.sigIgnoredAtEntry[name] {
		// A signal ignored when the shell started can be neither
		// trapped nor reset by a non-interactive shell — POSIX, and
		// bash's behavior (#441). Silently, as bash does: the script
		// asked for something the shell was told it may not do, and
		// the listing keeps saying the signal is ignored.
		return
	}
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

// nameRefElem splits a nameref target that names an array element into
// the array's name and its subscript, parsed as arithmetic.
func (r *Runner) nameRefElem(target string) (string, syntax.ArithmExpr, bool) {
	base, sub, ok := cutElemSubscript(target)
	if !ok {
		return "", nil, false
	}
	idx, err := syntax.NewParser().Arithmetic(strings.NewReader(sub))
	if err != nil || idx == nil {
		return "", nil, false
	}
	return base, idx, true
}

// withIndex restates an assignment as one to an array element.
func withIndex(as *syntax.Assign, index syntax.ArithmExpr) *syntax.Assign {
	out := *as
	out.Index = index
	return &out
}

// validNameRefTarget reports whether a nameref may point at target: a
// plain name, or a name with a subscript. Measured against bash 5.3 —
// `a[1]`, `a[$i]` and `a[@]` are all accepted, `a.b` and `foo bar` are
// not (#389).
func validNameRefTarget(target string) bool {
	if syntax.ValidName(target) {
		return true
	}
	base, _, ok := cutElemSubscript(target)
	return ok && syntax.ValidName(base)
}

// isCaseMode reports whether a declare mode is one of the -u/-l/-c
// case modifications, in either polarity.
func isCaseMode(mode string) bool {
	return len(mode) == 2 && (mode[1] == 'u' || mode[1] == 'l' || mode[1] == 'c')
}

// saveLocalOpts implements `local -`: the shell options are restored
// when the function returns, so a function may `set +e` without the
// caller inheriting it (#385).
func (r *Runner) saveLocalOpts() {
	if r.localOpts == nil {
		saved := r.opts
		r.localOpts = &saved
	}
}

// printDeclared writes one `declare -flags name=value` line, the form
// declare -p exists to produce: re-evaluable output (#383).
func (r *Runner) printDeclared(name string, vr expand.Variable) {
	flags := vr.Flags()
	if flags == "" {
		flags = "-"
	}
	// Declared but never set prints bare — `declare -a c` answers
	// `declare -a c`, not `=()`, and the same for a scalar or a
	// nameref (#378). The rule turns on Set rather than on the value
	// being empty: `foo=` *is* set, and prints `declare -- foo=""`.
	if !vr.Set {
		r.outf("declare -%s %s\n", flags, name)
		return
	}
	switch vr.Kind {
	case expand.Indexed:
		r.outf("declare -%s %s=(", flags, name)
		for i, v := range vr.List {
			if i > 0 {
				r.out(" ")
			}
			idx := i
			if vr.Indexes != nil {
				idx = vr.Indexes[i]
			}
			r.outf("[%d]=%s", idx, declQuote(v))
		}
		r.out(")\n")
	case expand.Associative:
		// Keys are sorted for determinism where bash prints its hash
		// order, and each element carries bash's trailing space:
		// ([one]="1" [two]="2" ).
		r.outf("declare -%s %s=(", flags, name)
		for _, k := range slices.Sorted(maps.Keys(vr.Map)) {
			r.outf("[%s]=%s ", declQuoteKey(k), declQuote(vr.Map[k]))
		}
		r.out(")\n")
	default:
		r.outf("declare -%s %s=%s\n", flags, name, declQuote(vr.Str))
	}
}

// declQuote renders a value the way bash's declare -p does, which is
// the whole point of that builtin: the output has to survive being
// re-read (#383). A control character forces ANSI-C quoting, since
// "a\nb" in double quotes is the two characters \ and n; otherwise the
// value is double-quoted with the four characters that would still
// expand — $ ` " \ — escaped, so `declare -p` of `$$` re-reads as the
// two dollars rather than the shell's pid.
func declQuote(s string) string {
	if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		var sb strings.Builder
		sb.WriteString("$'")
		for _, r := range s {
			switch r {
			case '\a':
				sb.WriteString(`\a`)
			case '\b':
				sb.WriteString(`\b`)
			case '\f':
				sb.WriteString(`\f`)
			case '\n':
				sb.WriteString(`\n`)
			case '\r':
				sb.WriteString(`\r`)
			case '\t':
				sb.WriteString(`\t`)
			case '\v':
				sb.WriteString(`\v`)
			case '\\', '\'':
				sb.WriteByte('\\')
				sb.WriteRune(r)
			default:
				if r < 0x20 || r == 0x7f {
					fmt.Fprintf(&sb, `\%03o`, r)
				} else {
					sb.WriteRune(r)
				}
			}
		}
		sb.WriteString("'")
		return sb.String()
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for i := range len(s) {
		switch c := s[i]; c {
		case '$', '`', '"', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// declQuoteKey renders an associative array key: bash leaves an
// ordinary key bare and quotes one that would not re-read as itself,
// so [a] stays [a] while [a b] becomes ["a b"].
func declQuoteKey(k string) string {
	plain := k != ""
	for i := range len(k) {
		if c := k[i]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == '/') {
			plain = false
			break
		}
	}
	if plain {
		return k
	}
	return declQuote(k)
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
	// Which descriptor this applies to. Input redirections default to 0 and
	// output ones to 1; "{name}>" allocates one and stores its number.
	//
	// The here-document forms are input redirections and take a
	// descriptor like any other (#414): `3<<E` opens fd 3, and koi used
	// to answer this before the descriptor was even worked out — every
	// body landed on fd 0, so `<<E1 3<<E2` left fd 3 unopened and put
	// E2's body on the shell's stdin.
	fd := 1
	switch rd.Op {
	case syntax.RdrIn, syntax.DplIn, syntax.RdrInOut,
		syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
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
	if rd.Hdoc != nil {
		pr, err := r.hdocReader(rd)
		if err != nil {
			return nil, err
		}
		if err := r.setInputFd(fd, pr); err != nil {
			return nil, err
		}
		r.setFdVar(fdVarName, fd)
		if fdVarName != "" {
			return nil, nil // see the tail of this function
		}
		return pr, nil
	}

	orig := &r.stdout
	if fd == 2 {
		orig = &r.stderr
	}
	// bash expands a redirection's word the way it expands any other —
	// splitting and globbing included — and then requires exactly one
	// field to come out (#415). `cat < g*` reads g1, while `cat < $z`
	// with z="a b", `cat < f*` matching two files, and `> $unset` are
	// all "ambiguous redirect" with status 1.
	//
	// koi did neither half: it took the word literally, so a glob opened
	// a file named `g*` and a two-word expansion opened one named "a b",
	// silently creating it on the output side.
	//
	// A here-string is the exception, being content rather than a
	// filename: `cat <<< $z` prints "a b" and `cat <<< h*` prints the
	// literal `h*` (both measured).
	var arg string
	if rd.Op == syntax.WordHdoc {
		arg = r.literal(rd.Word)
	} else {
		fields := r.fields(rd.Word)
		if len(fields) != 1 {
			// bash names the word as written rather than what it
			// expanded to, which is what makes the message useful.
			r.errf("%s: ambiguous redirect\n", wordSource(rd.Word))
			return nil, errAmbiguousRedirect
		}
		arg = fields[0]
	}
	// op is what the redirection *means*, which is not always what was
	// written: `>&file` is csh's "send both streams to this file", so it
	// becomes RdrAll below and takes the ordinary file path from there.
	op := rd.Op
	switch op {
	case syntax.WordHdoc:
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		// A here-string takes a descriptor too: `read -u 3 x 3<<< hi`.
		if err := r.setInputFd(fd, pr); err != nil {
			return nil, err
		}
		r.setFdVar(fdVarName, fd)
		// We write to the pipe in a new goroutine,
		// as pipe writes may block once the buffer gets full.
		go func() {
			pw.WriteString(arg)
			pw.WriteString("\n")
			pw.Close()
		}()
		if fdVarName != "" {
			return nil, nil // see the tail of this function
		}
		return pr, nil
	case syntax.DplOut:
		if arg == "-" {
			r.closeFd(fd)
			return nil, nil
		}
		// `>&N-` moves rather than copies: dup N onto this descriptor
		// and then close N (#417). Without it the swizzle loops that
		// hand a descriptor along leak every one they touch, which is
		// how vredir6.sub ran koi out of descriptors.
		moveSrc, move := strings.CutSuffix(arg, "-")
		if move {
			arg = moveSrc
		}
		src, err := strconv.Atoi(arg)
		if err != nil {
			// `>&file` with a word that is not a descriptor is csh's
			// both-streams form, and only without an explicit fd:
			// `ls >&out` writes stdout and stderr to out, while
			// `ls 2>&out` is ambiguous. Both measured (#416). koi used
			// to answer neither — the redirection was dropped with no
			// message and no file, so the output went to the terminal
			// and the script read a file that was never created.
			if rd.N == nil {
				op = syntax.RdrAll
				break
			}
			r.errf("%s: ambiguous redirect\n", wordSource(rd.Word))
			return nil, errAmbiguousRedirect
		}
		w := r.fdWriter(src)
		if w == nil {
			r.errf("%v: Bad file descriptor\n", src)
			return nil, errBadFd
		}
		r.setFdWriter(fd, w)
		if move && src != fd {
			// Closing the source is the whole difference from a plain
			// dup. `6>&6-` is a no-op rather than a self-close, which is
			// why the descriptors are compared first.
			r.closeFd(src)
		}
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
		// `<&N-` is the input side of the same move (#417).
		moveSrc, move := strings.CutSuffix(arg, "-")
		if move {
			arg = moveSrc
		}
		src, err := strconv.Atoi(arg)
		if err != nil {
			// There is no csh form on the input side: `<&word` is
			// ambiguous whatever the fd, which is also the answer for
			// `exec 3<&$fd` with fd unset (#415's dup half).
			r.errf("%s: ambiguous redirect\n", wordSource(rd.Word))
			return nil, errAmbiguousRedirect
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
		if move && src != fd {
			r.closeFd(src) // the move's other half; see DplOut above
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
		switch op {
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
	switch op {
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
	switch op {
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
	if fdVarName != "" {
		// Handing back no closer is the other half of keeping the
		// descriptor open (#418): the statement closes what it is given,
		// and a {varname} file must outlive it. It is closed by an
		// explicit `{fd}>&-`, or when the shell exits.
		return nil, nil
	}
	return f, nil
}

// setFdVar records the descriptor a "{name}>" redirection allocated.
func (r *Runner) setFdVar(name string, fd int) {
	if name == "" {
		return
	}
	// Remember it for the statement's restore, which is what keeps the
	// descriptor open past the command (#418).
	r.varRedirFds = append(r.varRedirFds, fd)
	// Through a nameref the descriptor lands on the *target*, the way
	// any other assignment does (#418). koi wrote the reference variable
	// itself, so `declare -n ref=target; exec {ref}</dev/null` clobbered
	// ref and left target unset — the reference destroyed rather than
	// followed.
	prev := r.lookupVar(name)
	if target, _ := prev.Resolve(r.writeEnv); target != "" {
		name = target
	}
	r.setVarString(name, strconv.Itoa(fd))
}

// wordSource renders a redirection's word as it was written, which is
// what bash names in an "ambiguous redirect" — `$z`, not the two fields
// it became.
func wordSource(w *syntax.Word) string {
	var sb strings.Builder
	if err := syntax.NewPrinter(syntax.SingleLine(true)).Print(&sb, w); err != nil {
		return ""
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// errAmbiguousRedirect is returned when a redirection's word cannot name
// a descriptor. Like errBadFd it is already reported when it is returned.
var errAmbiguousRedirect = errors.New("ambiguous redirect")

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

// expandAlias splices an alias's text into the command it heads and
// re-parses the result, which is how bash expands one. It answers
// ok=false when the head is not an alias, so the ordinary path runs.
//
// The re-parse is what makes the rest of the issue fall out: the
// replacement is scanned for aliases again (so `alias v="e 123"` finds
// `e`), a trailing blank in the replacement asks for the *next* word
// to be expanded too, and a newline inside the text terminates the
// command rather than joining two words.
func (r *Runner) expandAlias(cm *syntax.CallExpr) ([]*syntax.Stmt, string, bool) {
	name := cm.Args[0].Lit()
	als, ok := r.alias[name]
	if !ok || r.expandingAlias[name] {
		// A self-referential alias expands once and then stands for
		// the command, which is how `alias ls='ls --color'` works at
		// all.
		return nil, "", false
	}
	var sb strings.Builder
	for _, as := range cm.Assigns {
		printNode(&sb, as)
		sb.WriteString(" ")
	}
	sb.WriteString(als.text)
	rest := cm.Args[1:]
	// A replacement ending in a blank asks for the *next* word to be
	// alias-expanded as well, which is the idiom behind
	// `alias sudo='sudo '`.
	for als.blank && len(rest) > 0 {
		next, ok := r.alias[rest[0].Lit()]
		if !ok || r.expandingAlias[rest[0].Lit()] {
			break
		}
		sb.WriteString(" " + next.text)
		rest, als = rest[1:], next
	}
	for _, w := range rest {
		sb.WriteString(" ")
		printNode(&sb, w)
	}
	// Redirections belong to the statement rather than to the call, so
	// they are still in place around the spliced command and do not
	// need re-printing here.
	file, err := syntax.NewParser().Parse(strings.NewReader(sb.String()), "")
	if err != nil {
		// An alias whose text does not parse is a runtime error on the
		// line that used it, which is where bash reports it too.
		r.errf("%s\n", err)
		r.exit.code = 1
		return nil, name, true
	}
	return file.Stmts, name, true
}

// printNode renders one node back to source, which is how the spliced
// command line is rebuilt from the tree koi parsed.
func printNode(sb *strings.Builder, node syntax.Node) {
	syntax.NewPrinter().Print(sb, node) //nolint:errcheck // writing to a strings.Builder
}

// funSubst runs a bash 5.3 function substitution in this shell.
//
// The two spellings differ in where the value comes from, which is also
// why `${| …; }` does not capture: `${ …; }` is the command's output,
// while `${| …; }` is whatever the body left in REPLY, with the
// caller's REPLY saved and restored around it — measured.
func (r *Runner) funSubst(ctx context.Context, w io.Writer, cs *syntax.CmdSubst) error {
	// The value is written *last*, into a writer the expander hands us
	// that is a buffer it reuses. An ordinary substitution runs in a
	// subshell with its own expansion config, so it can write as it
	// goes; a funsub runs here, where a nested expansion resets the
	// very buffer being written to — which is how `${ echo a; echo b; }`
	// first came out as "bb".
	value := ""
	if cs.ReplyVar {
		prev := r.lookupVar("REPLY")
		r.delVar("REPLY")
		r.stmts(ctx, cs.Stmts)
		value = r.envGet("REPLY")
		if prev.IsSet() {
			r.setVar("REPLY", prev)
		} else {
			r.delVar("REPLY")
		}
	} else {
		var buf bytes.Buffer
		oldStdout := r.stdout
		r.stdout = &buf
		r.stmts(ctx, cs.Stmts)
		r.stdout = oldStdout
		value = buf.String()
	}
	if sb, ok := w.(*strings.Builder); ok {
		sb.Reset()
	}
	_, err := io.WriteString(w, value)
	return err
}

// splitKeywordAssigns separates the assignment-shaped words `set -k`
// binds from the words that stay arguments, working on the parsed word
// so that quoting decides: an assignment's name must be written as a
// bare literal, which is what makes `echo "x=1"` an argument.
//
// The command name is never a candidate; a leading assignment is
// already an Assign in the tree rather than an argument.
func splitKeywordAssigns(args []*syntax.Word) ([]*syntax.Word, []*syntax.Assign) {
	rest := args[:1:1]
	var kw []*syntax.Assign
	for _, word := range args[1:] {
		lit, ok := word.Parts[0].(*syntax.Lit)
		if !ok {
			rest = append(rest, word)
			continue
		}
		name, value, found := strings.Cut(lit.Value, "=")
		if !found || !syntax.ValidName(name) {
			rest = append(rest, word)
			continue
		}
		// The value keeps every part after the `=`, so `c=$x` and
		// `c="a b"` expand as they would in a leading assignment.
		parts := []syntax.WordPart{&syntax.Lit{Value: value}}
		parts = append(parts, word.Parts[1:]...)
		kw = append(kw, &syntax.Assign{
			Name:  &syntax.Lit{Value: name},
			Value: &syntax.Word{Parts: parts},
		})
	}
	return rest, kw
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

		// `local -` inside the body records the options to put back
		// when it returns (#385); each call gets its own slot, so a
		// nested function's save does not disturb this one's.
		oldLocalOpts := r.localOpts
		r.localOpts = nil

		// The getopts scan position travels with a local OPTIND: a
		// function that declares one gets its own scan and the caller
		// resumes where it was (#403). Without this the recursive
		// idiom in getopts8.sub never terminates.
		oldOptState := r.optState

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

		// Checked before the frame is popped, since that is when a
		// local OPTIND is still visible.
		hadLocalOptind := r.localInScope("OPTIND")

		r.writeEnv = origEnv

		r.errTrapFired, r.errTrapDepth = oldErrTrapFired, oldErrTrapDepth
		r.pipeStatusSet, r.pipeStatus = oldPipeStatusSet, oldPipeStatus
		popFrame()
		r.Params = oldParams
		r.inFunc = oldInFunc
		if r.localOpts != nil {
			r.opts = *r.localOpts
			r.updateExpandOpts()
		}
		r.localOpts = oldLocalOpts
		if hadLocalOptind {
			r.optState = oldOptState
		}
		r.exit.returning = false
		return
	}
	if IsBuiltin(name) && !r.disabledBuiltins[name] {
		r.exit = r.builtin(ctx, pos, name, args[1:])
		return
	}
	if path, ok := r.hashTable[name]; ok {
		// A hashed name runs the pinned program rather than whatever a
		// PATH search would find (#411).
		args = append([]string{path}, args[1:]...)
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
		f, err := os.OpenFile(path, flags, mode)
		if err != nil && os.IsNotExist(err) && flags&(os.O_WRONLY|os.O_RDWR) == 0 {
			// bash's process substitutions are /dev/fd entries, so
			// reading one a second time gives EOF rather than an error
			// (#420): `f() { wc -l < $1; wc -l < $1; }` prints 1 then
			// 0. koi's FIFO is gone by then, and the failed open
			// answered nothing at all — a missing line rather than a
			// zero.
			return os.Open(os.DevNull)
		}
		return f, err
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
