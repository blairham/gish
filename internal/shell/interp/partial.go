package interp

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/syntax"
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
	// The source is kept as it is read, so that relitHeredocs below can
	// take a here-document body back out of it verbatim. Positions on the
	// tree are offsets into this same stream, so they line up.
	var src bytes.Buffer
	var perr error
	for stmt, err := range syntax.NewParser().StmtsSeq(io.TeeReader(r, &src)) {
		if err != nil {
			perr = err
			break
		}
		relitHeredocs(stmt, src.Bytes())
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

// relitHeredocs puts a here-document body back the way it was written,
// when the delimiter says it must not be touched and the parser thought
// otherwise (#258).
//
// POSIX: "if any character in word is quoted, the here-document lines
// shall not be expanded". Any character, so `<<'X'Y` is a quoted
// delimiter. The parser decides the same question in unquotedWordBytes
// and overwrites its verdict per word part rather than accumulating it,
// so the trailing unquoted `Y` is the only part that counts and the whole
// delimiter reads as unquoted. The body is then lexed as an expanding
// one: `$V` becomes a real ParamExp, `\\` becomes an escape, and running
// it prints a value and eats a backslash from a heredoc whose quote
// promised neither.
//
// The repair is to replace that tree with the one literal it should have
// been, sliced out of the source the parser just read. Slicing rather
// than printing the tree back is the point: the printer canonicalizes,
// turning a backquote into $( ) and `$((1+1))` into `$((1 + 1))`, so a
// heredoc written to hold text exactly would have been rewritten on its
// way to disk -- the same class of silent damage this fixes, arriving
// from the other side.
//
// Doing it here rather than at the point of use is what makes it reach
// `source` and `eval`, which parse again inside the interpreter: they
// come through ParseAsRead too, which is the gap that #288 had to close
// for the fully quoted form.
func relitHeredocs(stmt *syntax.Stmt, src []byte) {
	syntax.Walk(stmt, func(n syntax.Node) bool {
		rd, ok := n.(*syntax.Redirect)
		if !ok || rd.Hdoc == nil || !posixQuotedDelim(rd.Word) {
			return true
		}
		if len(rd.Hdoc.Parts) == 1 {
			if _, isLit := rd.Hdoc.Parts[0].(*syntax.Lit); isLit {
				return true // the parser already agreed; nothing to repair
			}
		}
		body, ok := heredocSource(rd, src)
		if !ok {
			return true
		}
		rd.Hdoc = &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{
			ValuePos: rd.Hdoc.Pos(),
			ValueEnd: rd.Hdoc.End(),
			Value:    body,
		}}}
		return true
	})
}

// heredocSource slices a here-document body out of the source it was
// parsed from.
//
// The body's End position sits after the closing delimiter rather than
// before it -- consistently, in every form -- so the delimiter's own text
// comes off the end. That the slice really does end with the delimiter is
// checked rather than assumed: if it does not, the offsets are not what
// this reasoning expects and the body is left alone, because a wrong
// slice here would corrupt the very text the fix exists to protect.
func heredocSource(rd *syntax.Redirect, src []byte) (string, bool) {
	lo, hi := rd.Hdoc.Pos().Offset(), rd.Hdoc.End().Offset()
	if lo > hi || hi > uint(len(src)) {
		return "", false
	}
	body := string(src[lo:hi])
	delim := delimText(rd.Word)
	if delim == "" || !strings.HasSuffix(body, delim) {
		return "", false
	}
	return strings.TrimSuffix(body, delim), true
}

// delimText is the delimiter as the lexer matches it: with the quoting
// that made it a delimiter removed.
func delimText(w *syntax.Word) string {
	var sb strings.Builder
	for _, part := range w.Parts {
		switch part := part.(type) {
		case *syntax.SglQuoted:
			sb.WriteString(part.Value)
		case *syntax.DblQuoted:
			for _, inner := range part.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "" // an expansion in a delimiter is not ours to guess at
				}
				sb.WriteString(lit.Value)
			}
		case *syntax.Lit:
			sb.WriteString(strings.ReplaceAll(part.Value, "\\", ""))
		default:
			return ""
		}
	}
	return sb.String()
}

// posixQuotedDelim reports whether any character of a here-document
// delimiter is quoted, which is POSIX's test for a body that must not
// expand. See relitHeredocs for why the parser answers differently.
func posixQuotedDelim(w *syntax.Word) bool {
	if w == nil || len(w.Parts) == 0 {
		return false
	}
	for _, part := range w.Parts {
		switch part := part.(type) {
		case *syntax.SglQuoted, *syntax.DblQuoted:
			return true
		case *syntax.Lit:
			if strings.Contains(part.Value, "\\") {
				return true
			}
		}
	}
	return false
}
