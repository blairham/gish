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
			r2.stdout = w
			r2.stmts(ctx, cs.Stmts)
			r2.exit.exiting = false // subshells don't exit the parent shell
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
				r2.exit.exiting = false // subshells don't exit the parent shell
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
	r.ecfg.NoUnset = r.opts[optNoUnset]
	r.ecfg.ExtGlob = r.opts[optExtGlob]
}

func (r *Runner) expandErr(err error) {
	if err == nil {
		return
	}
	errMsg := err.Error()
	fmt.Fprintln(r.stderr, errMsg)
	switch {
	case errors.As(err, &expand.UnsetParameterError{}):
	case errMsg == "invalid indirect expansion":
		// TODO: These errors are treated as fatal by bash.
		// Make the error type reflect that.
	default:
		return // other cases do not exit
	}
	r.exit.code = 1
	r.exit.exiting = true
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
	// Some traps trigger on exit, so we do want those to run.
	if !r.handlingTrap && (r.exit.returning || r.exit.exiting) {
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
	if r.stop(ctx) {
		return
	}
	r.exit = exitStatus{}
	if st.Background || st.Disown {
		r2 := r.subshell(true)
		st2 := *st
		st2.Background = false
		st2.Disown = false
		bg := bgProc{
			done: make(chan struct{}),
			exit: new(exitStatus),
		}
		r.bgProcs = append(r.bgProcs, bg)
		go func() {
			r2.Run(ctx, &st2)
			r2.exit.exiting = false // subshells don't exit the parent shell
			*bg.exit = r2.exit
			close(bg.done)
		}()
	} else {
		r.stmtSync(ctx, st)
	}
	r.lastExit = r.exit
}

// setFuncName publishes the call stack. Note that bash leaves the variable
// unset rather than empty at the top level, so a script can tell "not in a
// function" from a function whose name happens to be empty.
func (r *Runner) setFuncName() {
	if len(r.funcStack) == 0 {
		r.delVar(shellFuncNameVar)
		return
	}
	r.setVar(shellFuncNameVar, expand.Variable{
		Set:  true,
		Kind: expand.Indexed,
		List: slices.Clone(r.funcStack),
	})
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
			r.trapCallback(ctx, r.callbackErr, "error")
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
		r2.exit.exiting = false // subshells don't exit the parent shell
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
					r.errf("%s: readonly variable\n", name)
					r.exit.code = 1
					r.exit.exiting = true
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
			r2 := r.subshell(true)
			r2.stdout = pw
			if cm.Op == syntax.PipeAll {
				r2.stderr = pw
			} else {
				r2.stderr = r.stderr
			}
			oldIn := r.stdin
			r.stdin = pr
			var wg sync.WaitGroup
			wg.Go(func() {
				r2.stmt(ctx, cm.X)
				r2.exit.exiting = false // subshells don't exit the parent shell
				pw.Close()
			})
			r.pipeStatus = nil
			r.stmt(ctx, cm.Y)
			pr.Close()
			wg.Wait()
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
			r.stdin = oldIn
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

					line, err := r.readLine(ctx, true, '\n', -1, false)
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
			for y.Cond == nil || r.arithm(y.Cond) != 0 {
				if !r.exit.ok() || r.loopStmtsBroken(ctx, cm.Do) {
					break
				}
				if y.Post != nil {
					r.arithm(y.Post)
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
		local, global := false, false
		var modes []string
		valType := ""
		declQuery := "" // "-f", "-F" or "-p" for query mode
		namedAny := false
		switch cm.Variant.Value {
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
		for as := range r.flattenAssigns(cm.Args) {
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
				case "-g":
					global = true
				case "-f", "-p", "-F":
					declQuery = flag
				default:
					r.errf("declare: invalid option %q\n", flag)
					r.exit.code = 2
					return
				}
			}
			if sawFlag {
				continue assignLoop
			}
			name := as.Name.Value
			namedAny = true
			if !syntax.ValidName(name) {
				r.errf("declare: invalid name %q\n", name)
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
					r.errf("declare: %s: not found\n", name)
					r.exit.code = 1
					continue
				}
				flags := vr.Flags()
				if flags == "" {
					flags = "-"
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
						r.outf("[%d]=%q", idx, v)
					}
					r.out(")\n")
				case expand.Associative:
					r.outf("declare -%s %s=(", flags, name)
					first := true
					for k, v := range vr.Map {
						if !first {
							r.out(" ")
						}
						r.outf("[%s]=%q", k, v)
						first = false
					}
					r.out(")\n")
				default:
					r.outf("declare -%s %s=%q\n", flags, name, vr.Str)
				}
				continue
			}
			vr := r.lookupVar(name)
			// The integer attribute has to be settled before the value is
			// computed, since it is what decides whether the value is an
			// arithmetic expression; the other modes are applied afterwards.
			if slices.Contains(modes, "-i") {
				vr.Integer = true
			} else if slices.Contains(modes, "+i") {
				vr.Integer = false
			}
			if as.Naked {
				if valType == "-A" {
					vr.Kind = expand.Associative
				} else {
					vr.Kind = expand.KeepValue
				}
			} else {
				name, vr = r.assignVal(name, vr, as, valType)
			}
			if global {
				vr.Local = false
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
			r.setVar(name, vr)
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

// printFuncDef prints a function's definition, as "declare -f" does. Note that
// the layout differs from bash's, which indents the body over several lines.
func (r *Runner) printFuncDef(name string, body *syntax.Stmt) {
	r.outf("%s()\n", name)
	printer := syntax.NewPrinter()
	var buf bytes.Buffer
	printer.Print(&buf, body)
	r.outf("%s\n", buf.String())
}

func (r *Runner) trapCallback(ctx context.Context, callback, name string) {
	if callback == "" {
		return // nothing to do
	}
	if r.handlingTrap {
		return // don't recurse, as that could lead to cycles
	}
	r.handlingTrap = true
	defer func() { r.handlingTrap = false }()

	p := syntax.NewParser()
	// TODO: do this parsing when "trap" is called?
	file, err := p.Parse(strings.NewReader(callback), name+" trap")
	if err != nil {
		r.errf(name+"trap: %v\n", err)
		// ignore errors in the callback
		return
	}
	oldExit, oldLastExit := r.exit, r.lastExit
	r.lastExit = r.exit
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
	r.stmts(ctx, file.Stmts)
	r.errTrapFired = oldErrTrapFired
	r.pipeStatusSet, r.pipeStatus = oldPipeStatusSet, oldPipeStatus
	if oldPipeStatusVar.Set {
		r.setVar(shellPipeStatusVar, oldPipeStatusVar)
	}
	r.exit, r.lastExit = oldExit, oldLastExit // traps on EXIT or ERR should not modify the result
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
	_ = err // TODO: report these errors
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
	// The quoted delimiter sends the lexer down its own path
	// ([syntax.Parser.quotedHdocWord]), which yields exactly one literal
	// and no expansion tree to preserve. Any other shape means the parser
	// did *not* agree the delimiter was quoted, and treating live
	// expansions as text is the same bug pointing the other way — so leave
	// it to the expanding path.
	if len(rd.Hdoc.Parts) != 1 {
		return "", false
	}
	lit, ok := rd.Hdoc.Parts[0].(*syntax.Lit)
	if !ok {
		return "", false
	}
	// `<<-'X'` strips leading tabs from every line. The lexer skips them
	// only when matching the delimiter, not when building the body, so the
	// stripping is the consumer's job either way — see the DashHdoc branch
	// in hdocReader, which does the same thing a line at a time because it
	// has expansions to interleave and this does not.
	if rd.Op != syntax.DashHdoc {
		return lit.Value, true
	}
	lines := strings.Split(lit.Value, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimLeft(line, "\t")
	}
	return strings.Join(lines, "\n"), true
}

// hdocDelimQuoted reports whether the *parser* treated this
// here-document's delimiter as quoted.
//
// The parser decides this in unquotedWordBytes and keeps the verdict to
// itself — nothing on the tree records it — so it has to be recomputed
// here, quirk included: that function overwrites its verdict per part
// instead of accumulating it, so only the last part decides and `<<'X'Y`
// reads as unquoted (#258). Matching the parser is the point rather than
// an oversight, because that verdict is what picked the lexer path which
// built the body; being more correct than it here would mean handing back
// a body that still has real expansions in it.
func hdocDelimQuoted(w *syntax.Word) bool {
	if w == nil || len(w.Parts) == 0 {
		return false
	}
	switch part := w.Parts[len(w.Parts)-1].(type) {
	case *syntax.SglQuoted, *syntax.DblQuoted:
		return true
	case *syntax.Lit:
		return strings.Contains(part.Value, "\\")
	}
	return false
}

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

		oldFuncStack := r.funcStack
		r.funcStack = append([]string{name}, r.funcStack...)
		r.setFuncName()

		// Functions run in a nested scope.
		// Note that [Runner.exec] below does something similar.
		origEnv := r.writeEnv
		r.writeEnv = &overlayEnviron{parent: r.writeEnv, funcScope: true}

		r.stmt(ctx, body)

		r.writeEnv = origEnv

		r.errTrapFired, r.errTrapDepth = oldErrTrapFired, oldErrTrapDepth
		r.pipeStatusSet, r.pipeStatus = oldPipeStatusSet, oldPipeStatus
		r.funcStack = oldFuncStack
		r.setFuncName()
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
