package interp

import (
	"errors"
	"io"

	"mvdan.cc/sh/v3/syntax"
)

// Reading a script the way bash does (#276).
//
// bash does not parse a file and then run it. It reads a line, parses
// what that line completes, runs it, and reads the next — so a syntax
// error on line 129 is reported at line 129 with the first 128 lines
// already run and their side effects standing. Parsing up front instead
// makes one unreadable construct anywhere in a file discard the whole
// file, which is the difference between a user's rc losing its last line
// and losing all of it.
//
// SyntaxErrorStatus is what every non-interactive form exits with when
// the parse fails: a script, -c, a sourced file, and eval all answer 2.
// koi's own -n check already used it (repl.NoExecStatus); a real run
// answered 1, which is the same claim spelled two ways.
const SyntaxErrorStatus = 2

// ParseAsRead parses r a statement at a time and returns the statements
// bash would already have run when the first error stopped it.
//
// The cut is by *line*, not by statement, because bash's reading unit is
// the line: `echo a; if then fi` runs nothing, while the same two
// commands on separate lines run the first. Keeping every statement that
// ends before the error's line is that rule, and it holds for a compound
// command spanning many lines — the error inside an `if` block discards
// the whole `if`, since the `if` statement never completed and so was
// never yielded.
//
// A nil error means the whole input parsed and stmts is all of it, which
// is the ordinary case and the one that must stay exactly as it was.
func ParseAsRead(r io.Reader, name string) (stmts []*syntax.Stmt, _ error) {
	var perr error
	for stmt, err := range syntax.NewParser().StmtsSeq(r) {
		if err != nil {
			perr = err
			break
		}
		stmts = append(stmts, stmt)
	}
	if perr == nil {
		return stmts, nil
	}
	// StmtsSeq has no name to put in the error, so a diagnostic would
	// otherwise say "2:1:" where Parse says "lib.sh:2:1:".
	var pe syntax.ParseError
	if errors.As(perr, &pe) {
		if pe.Filename == "" && name != "" {
			pe.Filename = name
			perr = pe
		}
		stmts = runnableBefore(stmts, pe.Pos.Line())
	}
	return stmts, perr
}

// runnableBefore drops the statements that share a line with the error,
// which bash never got to run because it could not finish reading that
// line.
func runnableBefore(stmts []*syntax.Stmt, line uint) []*syntax.Stmt {
	for i, stmt := range stmts {
		if stmt.End().Line() >= line {
			return stmts[:i]
		}
	}
	return stmts
}
