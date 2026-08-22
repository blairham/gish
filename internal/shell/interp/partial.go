package interp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
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

// ScriptReader reads a script the way bash reads one: a line at a time,
// so that running a line can change how the next is read.
//
// That is what a shell needs and a parse-it-all-up-front reader cannot
// give it. `set -o posix` changes how the rest of the script is
// *tokenized* (#450), and `exec 0< file` changes where the rest of the
// script is *read from* when the shell reads its commands from standard
// input (#516) — both are decided between lines, which is why the line
// is the unit here rather than the statement. bash reads a whole line
// before running any of it, so `set -o posix; echo "${x+\'}"` on one
// line still tokenizes the second command the old way; keeping a line's
// statements together is what reproduces that.
type ScriptReader struct {
	parser *syntax.Parser
	rd     io.Reader
	name   string
	filter LineFilter
	// src is everything read so far. Statement positions are offsets
	// into it, so a caller can slice a statement's source out of it —
	// which is what ambient history recording does, and what
	// relitHeredocs below needs.
	src bytes.Buffer
}

// NewScriptReader prepares to read a script from r. name appears in
// parse errors.
func NewScriptReader(r io.Reader, name string, opts ...syntax.ParserOption) *ScriptReader {
	return &ScriptReader{parser: syntax.NewParser(opts...), rd: r, name: name}
}

// Configure applies parser options to the live parser, which is how a
// mode a script turns on mid-run reaches the rest of it. Call it between
// lines: applying an option while a line is being tokenized would split
// that line between two modes, which is not what any shell does.
func (sr *ScriptReader) Configure(opts ...syntax.ParserOption) {
	for _, opt := range opts {
		opt(sr.parser)
	}
}

// Source is everything read so far.
func (sr *ScriptReader) Source() string { return sr.src.String() }

// LineFilter rewrites one physical input line — its newline included —
// on its way from the script to the parser. num is the line's number in
// this stream, counting from 1, which is what a diagnostic about the line
// has to name.
//
// The replacement is what the parser reads and therefore what
// [ScriptReader.Source] reports, so a caller which records history
// records the rewritten line, as bash does. Returning the empty string
// refuses the line: nothing is parsed from it, nothing runs, reading
// carries on, and — because the parser never sees its newline — every
// later line numbers one lower, which is exactly what bash does with a
// line its own history expansion emptied.
type LineFilter func(line string, num int) string

// Filter installs a line filter, the seam for a transformation of the
// *line* rather than of the tree.
//
// History expansion is the one bash has: `!!` is replaced before the
// line is parsed, which is why it applies inside a double-quoted string
// and not inside a single-quoted one. It belongs to the shell around the
// interpreter — the history list is the session's — so this reader only
// provides the boundary, one line at a time and never inside a
// here-document body.
func (sr *ScriptReader) Filter(f LineFilter) { sr.filter = f }

// Lines yields the statements of each input line as it is read.
//
// A parse error is yielded once, with no statements, and ends the
// iteration: the statements sharing the error's line are dropped,
// because bash could not finish reading that line and so never ran them.
//
// A *recoverable* error is yielded the same way and reading continues
// (#581), which is bash's own recovery: the line is discarded and the
// next one is read. The caller reports it and keeps going — see
// [syntax.ParseError.Recoverable].
func (sr *ScriptReader) Lines() iter.Seq2[[]*syntax.Stmt, error] {
	return func(yield func([]*syntax.Stmt, error) bool) {
		var line []*syntax.Stmt
		src := sr.rd
		if sr.filter != nil {
			src = &filteredLines{rd: sr.rd, filter: sr.filter, inHdoc: sr.parser.InHereDoc}
		}
		for stmt, err := range sr.parser.StmtsSeq(io.TeeReader(src, &sr.src)) {
			if err != nil {
				yield(nil, sr.named(err))
				return
			}
			relitHeredocs(stmt, sr.src.Bytes())
			line = append(line, stmt)
			if !sr.parser.AtLineEnd() {
				continue // more of this line to read
			}
			// Asked once the line is complete rather than at the moment
			// of the error, because the unit bash throws away is the
			// line: an error inside a one-line `for` loop costs the
			// whole loop, and a statement earlier on the line never runs.
			if rec := sr.parser.RecoverableErrors(); len(rec) > 0 {
				line = nil
				if !yield(nil, sr.named(rec[0])) {
					return
				}
				continue
			}
			if !yield(line, nil) {
				return
			}
			line = nil
		}
		// The input can end mid-line — `x=(a &` with no newline after
		// it — and the error still belongs to the caller.
		if rec := sr.parser.RecoverableErrors(); len(rec) > 0 {
			yield(nil, sr.named(rec[0]))
		}
	}
}

// FilterLines wraps r so that each physical line passes through f on its
// way to whatever reads it, which is the same seam [ScriptReader.Filter]
// installs — exported because one reading path is not a ScriptReader.
//
// The shell reads its commands from standard input through
// [syntax.Parser.InteractiveSeq], whose Incomplete callback is the only
// thing that can print a continuation prompt under `-i`, so that loop
// drives the parser directly and had no line boundary to hang history
// expansion on (#694). inHdoc is the parser's own InHereDoc: a
// here-document's body is the document's text rather than the shell's
// input, and bash hands it over untouched.
func FilterLines(r io.Reader, f LineFilter, inHdoc func() bool) io.Reader {
	return &filteredLines{rd: r, filter: f, inHdoc: inHdoc}
}

// filteredLines hands the parser one physical line at a time, each one
// through a [LineFilter] first.
//
// The line is the unit because it is bash's: the option a filter reads
// can be turned on by the line before, and never by the line itself. The
// parser makes that exact granularity available for free — it reads no
// further than the newline that ends the statement it is about to yield,
// so the filter for line N+1 runs after line N has run. Measured, and
// re-measured for a compound command, a backslash continuation and a
// here-document, because the whole design rests on it.
//
// Reads from the underlying stream keep the shape they had without a
// filter — one read of the parser's own buffer size, never more — so a
// script which *is* the shell's standard input does not lose bytes its
// commands were going to read.
type filteredLines struct {
	rd     io.Reader
	filter LineFilter
	inHdoc func() bool
	raw    []byte // read from rd, not yet given to the filter
	out    []byte // filtered, not yet given to the parser
	num    int    // physical lines handed over so far
	err    error  // rd's, held until everything read has been handed over
}

func (f *filteredLines) Read(p []byte) (int, error) {
	for len(f.out) == 0 {
		i := bytes.IndexByte(f.raw, '\n')
		if i < 0 {
			if f.err == nil {
				buf := make([]byte, len(p))
				n, err := f.rd.Read(buf)
				f.raw = append(f.raw, buf[:n]...)
				f.err = err
				continue
			}
			if len(f.raw) == 0 {
				return 0, f.err
			}
			// The last line of a file which does not end in a newline.
			i = len(f.raw) - 1
		}
		line := string(f.raw[:i+1])
		f.raw = f.raw[i+1:]
		f.num++
		// A here-document's body is the document's text, not the shell's
		// input: bash hands it over untouched.
		if !f.inHdoc() {
			line = f.filter(line, f.num)
		}
		f.out = []byte(line)
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

// Recoverable reports whether err is a parse error bash discards an
// input unit for and then carries on from, which is the caller's signal
// to report it and keep reading (#581).
func Recoverable(err error) bool {
	var pe syntax.ParseError
	return errors.As(err, &pe) && pe.Recoverable
}

// named puts the script's name in a parse error, which StmtsSeq has no
// way to do: a diagnostic would otherwise say "2:1:" where Parse says
// "lib.sh:2:1:".
func (sr *ScriptReader) named(err error) error {
	var pe syntax.ParseError
	if errors.As(err, &pe) && pe.Filename == "" && sr.name != "" {
		pe.Filename = sr.name
		return pe
	}
	return err
}

// runReading runs a script as it reads it, and returns the parse error
// that stopped it, if any, after everything readable before it has run.
//
// This is `source`'s reader as well as a plain script's, which is what
// makes `set -o posix` in a sourced file reach the rest of that file —
// the #259 lesson that a fix belongs at the point of use, since a
// sourced file is parsed here rather than by the shell around the
// interpreter.
func (r *Runner) runReading(ctx context.Context, sr *ScriptReader) error {
	// A file has its own line numbers whatever it was reached from, so a
	// `source` inside an eval'd string does not inherit the string's
	// shift (#571).
	oldLine, oldExpand := r.lineOffset, uint64(0)
	r.lineOffset = 0
	if r.ecfg != nil {
		oldExpand, r.ecfg.LineOffset = r.ecfg.LineOffset, 0
	}
	defer func() {
		r.lineOffset = oldLine
		if r.ecfg != nil {
			r.ecfg.LineOffset = oldExpand
		}
	}()
	for stmts, err := range sr.Lines() {
		if err != nil {
			if Recoverable(err) {
				// The line is gone and the file carries on, which is
				// what bash does with a sourced file too (#581).
				r.ReportRecovered(err)
				continue
			}
			return err
		}
		r.stmts(ctx, stmts)
		// An aborting error ends the *line*, not the file (#585). That
		// is what the category means (#469) — abandon this input unit
		// and go back to reading — and in a file the unit is the line
		// (#450), which is what the shell's own loop does through
		// stmtsTopLevel. Ending the file here cost a sourced library
		// every line after a bad substitution, and cost the *caller* the
		// rest of its own line too, since the flag escaped with the
		// source's status. It was invisible because every case that
		// aborted did so on a file's last line, where "stop reading" and
		// "read the next line" look identical.
		//
		// r.stmts has already stopped at the aborting statement, so the
		// rest of the line is abandoned; clearing the flag is what lets
		// the next line be read.
		if r.exit.aborting {
			r.exit.aborting = false
			continue
		}
		// Stop reading, not just running: an `exit`, a `return` out of a
		// sourced file or a cancelled context all end the script here.
		// `set -n` deliberately does not — reading the rest is the whole
		// point of it.
		if r.exit.exiting || r.exit.returning || ctx.Err() != nil {
			return nil
		}
		sr.Configure(r.ParserOptions()...)
	}
	return nil
}

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
	for line, err := range NewScriptReader(r, name).Lines() {
		if err != nil {
			return stmts, err
		}
		stmts = append(stmts, line...)
	}
	return stmts, nil
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
