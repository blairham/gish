package repl

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// Reading a script the way bash reads one (#450, #516).
//
// bash does not read a script and then run it. It reads a line, runs it,
// and reads the next — and because it does, a line can change how the
// rest of the script is read. Two do:
//
//   - `set -o posix` changes how the rest of the script is *tokenized*:
//     a single quote inside a double-quoted ${...} stops being the start
//     of a quoted span, so `"${IFS+'bar} baz"` is an ordinary word where
//     it was a scan to EOF looking for a closing quote.
//   - `exec 0< file` changes where the rest of the script is *read
//     from*, when the shell reads its commands from standard input. The
//     rest of the script comes from the new file.
//
// Both are decided between lines rather than between statements, which
// is why the line is the unit. bash reads a whole line before running
// any of it, so `set -o posix; echo "${x+'}"` on one line still reads
// the second command the old way, and `exec 0< f; echo tail` runs the
// tail before the stream switches. Measured, both of them.
//
// The switch is detected by asking the runner which file its descriptor
// 0 now refers to, not by watching for the `exec` builtin: `exec 0< f`
// is one spelling, `exec <f` another, and a `{ …; } < f` around the
// whole script a third.

// scriptStream runs a script as the shell reads it.
type scriptStream struct {
	runner *interp.Runner
	name   string
	rec    *historyRecorder
	// src is the command stream, and stdin is what the runner's fd 0
	// pointed at when it was opened. They are compared after every line:
	// following the switch is only right when the script *is* the
	// shell's standard input, which is what `koi < script` is and what a
	// named script file is not.
	src   io.Reader
	stdin *os.File
}

// newScriptStream prepares to run src as the runner's script. When src
// is the file the runner reads standard input from, redirecting fd 0
// switches the command stream.
func newScriptStream(runner *interp.Runner, src io.Reader, name string, rec *historyRecorder) *scriptStream {
	s := &scriptStream{runner: runner, name: name, rec: rec, src: src}
	if f, ok := src.(*os.File); ok && f == runner.Stdin() {
		s.stdin = f
	}
	return s
}

// run reads and runs until the input ends, the shell exits, or a parse
// error stops it.
//
// The returned error is the script's, the way [interp.Runner.Run]
// returns one for a whole file: an exit status, or a real failure. perr
// is the parse error which stopped the read, if any — reported by the
// caller, since what a shell does with it differs by path.
func (s *scriptStream) run(ctx context.Context) (err error, perr error) {
	// A line that failed for a reason which is not an exit status — a
	// contained panic, most of all (#217) — must still be the answer
	// when the script ends. Reading line by line means a later line can
	// return a status of its own, and a koi bug that scrolled past would
	// be exactly the silent failure the guard exists to prevent.
	var fatal error
	defer func() {
		if fatal != nil {
			err = fatal
		}
	}()
	for {
		reader := interp.NewScriptReader(s.src, s.name, s.runner.ParserOptions()...)
		// History expansion is a transformation of the line, applied
		// before parsing, so it hangs off the same boundary a mode does
		// (#559) — one step earlier, on the raw text rather than on the
		// parser's options.
		reader.Filter(historyExpandFilter(s.runner))
		s.rec.restart(reader.Source)
		switched := false
		for stmts, rerr := range reader.Lines() {
			if rerr != nil {
				if interp.Recoverable(rerr) {
					// bash discards the line and reads the next one
					// (#581). The runner reports it, because the
					// diagnostic is located the way its runtime ones
					// are and the status a discarded line leaves is its
					// to set.
					s.runner.ReportRecovered(rerr)
					continue
				}
				perr = rerr
				break
			}
			for _, st := range stmts {
				rewriteSubstrateGaps(st)
			}
			s.rec.addLine(stmts)
			err = safely("running "+s.name, func() error {
				return s.runner.RunStmts(ctx, s.name, stmts)
			})
			if _, isStatus := errors.AsType[interp.ExitStatus](err); err != nil && !isStatus && fatal == nil {
				fatal = err
			}
			if s.runner.Exited() {
				// The exit trap has already run, so the script is over
				// and there is nothing left to finish.
				return err, perr
			}
			// A mode the line turned on reaches the next line, which is
			// bash's granularity for it.
			reader.Configure(s.runner.ParserOptions()...)
			if next := s.switched(); next != nil {
				// Whatever this reader had buffered belongs to the old
				// stream, which nothing will read again.
				s.src, s.stdin, switched = next, next, true
				break
			}
		}
		if !switched {
			break
		}
	}
	// The input ran out, which is the moment the EXIT trap fires and the
	// script's status is settled.
	err = safely("finishing "+s.name, func() error { return s.runner.Finish(ctx) })
	return err, perr
}

// switched reports the file the command stream has moved to, or nil if
// it has not moved.
func (s *scriptStream) switched() *os.File {
	if s.stdin == nil {
		return nil // this script is not the shell's standard input
	}
	if cur := s.runner.Stdin(); cur != nil && cur != s.stdin {
		return cur
	}
	return nil
}
