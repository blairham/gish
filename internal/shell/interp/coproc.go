package interp

import (
	"context"
	"os"
	"strconv"

	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/shell/expand"
)

// coproc runs a command asynchronously with a two-way pipe to it (#287).
//
// It is the other half of writing a bounded parallel loop in bash, the
// half `wait -n` does not cover: a long-lived helper process the script
// talks to over a pair of descriptors, rather than N short-lived jobs.
//
// Before this, the clause parsed and the executor dropped it — the shape
// #215 named as the expensive one. The status was 1 and `set -e` did stop,
// so it was not wholly silent, but a script without `set -e` carried on
// with the name unset and failed later at the first read from `${C[0]}`,
// which points at the wrong line.
//
// # The descriptors
//
// `${NAME[0]}` reads what the command writes; `${NAME[1]}` writes what it
// reads. They are allocated from [Runner.freeFd], which starts at 10 as
// bash does for `{varname}>`, so a script's own numbered descriptors are
// left alone. bash picks much higher numbers, and nothing may depend on
// the value — a script that hardcodes one instead of expanding the array
// is broken against real bash too.
//
// The command's own copy of the runner is made *before* the descriptors
// are registered, so the coprocess does not inherit the two ends the
// parent holds. bash does leak them into the child, which is the classic
// coproc deadlock — a coprocess that never sees EOF because it is holding
// the write end open itself. There is no compatibility argument for
// reproducing a hang.
func (r *Runner) coproc(ctx context.Context, cc *syntax.CoprocClause) {
	// bash's own rule, and the parser already applies it: a name is only
	// accepted when the command is a compound. `coproc NAME cmd a b` is
	// parsed as the simple command `NAME cmd a b`, named COPROC.
	name := "COPROC"
	if cc.Name != nil {
		name = r.literal(cc.Name)
	}
	if !syntax.ValidName(name) {
		r.errf("coproc: %q: not a valid name\n", name)
		r.exit.code = 1
		return
	}

	// toChild carries what the script writes; fromChild carries what the
	// command writes back.
	childStdin, parentWrite, err := os.Pipe()
	if err != nil {
		r.errf("coproc: %v\n", err)
		r.exit.code = 1
		return
	}
	parentRead, childStdout, err := os.Pipe()
	if err != nil {
		childStdin.Close()
		parentWrite.Close()
		r.errf("coproc: %v\n", err)
		r.exit.code = 1
		return
	}

	// Made here, before the descriptors below exist, so the coprocess
	// does not inherit them. See the deadlock note above.
	r2 := r.subshell(true)
	r2.stdin = childStdin
	r2.stdout = childStdout

	readFd := r.freeFd()
	r.setFdFile(readFd, parentRead)
	writeFd := r.freeFd()
	r.setFdFile(writeFd, parentWrite)

	r.setVar(name, expand.Variable{
		Set:  true,
		Kind: expand.Indexed,
		List: []string{strconv.Itoa(readFd), strconv.Itoa(writeFd)},
	})

	bg := bgProc{
		done: make(chan struct{}),
		exit: new(exitStatus),
	}
	r.bgProcs = append(r.bgProcs, bg)
	// NAME_PID is what a script waits on, and it has to spell the pid the
	// same way $! does — "g" plus the 1-indexed position — or `wait
	// "$NAME_PID"` would not find the job this just started.
	r.setVarString(name+"_PID", "g"+strconv.Itoa(len(r.bgProcs)))

	go func() {
		defer func() {
			// Closing the command's ends is what lets the script's read
			// see EOF and its write see a broken pipe. Nothing else does
			// it: these are in-process files, not a forked child's copies.
			childStdin.Close()
			childStdout.Close()
			*bg.exit = r2.exit
			close(bg.done)
		}()
		r2.stmt(ctx, cc.Stmt)
		r2.exit.exiting = false  // subshells don't exit the parent shell
		r2.exit.aborting = false // nor unwind it: an abort inside a subshell ends that subshell
	}()

	// Starting a coprocess succeeds; whether the command does is what
	// NAME_PID is for.
	r.exit = exitStatus{}
}
