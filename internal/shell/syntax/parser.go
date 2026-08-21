// Copyright (c) 2016, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package syntax

import (
	"fmt"
	"io"
	"iter"
	"math/bits"
	"slices"
	"strings"
	"unicode/utf8"
)

// ParserOption is a function which can be passed to NewParser
// to alter its behavior. To apply option to existing Parser
// call it directly, for example KeepComments(true)(parser).
type ParserOption func(*Parser)

// KeepComments makes the parser parse comments and attach them to
// nodes, as opposed to discarding them.
func KeepComments(enabled bool) ParserOption {
	return func(p *Parser) { p.keepComments = enabled }
}

// POSIXMode makes the parser follow bash's `set -o posix` where posix
// mode changes how a script is *tokenized*.
//
// It is not [Variant](LangPOSIX), which selects the POSIX shell language:
// bash in posix mode still has `[[ ]]`, arrays and `$'…'`, and only a
// handful of its rules move. What moves here is the one that a shell
// which parses ahead cannot reach any other way — a single quote inside
// a double-quoted `${...}` is an ordinary character rather than the
// start of a quoted span (#450).
//
// Because a script turns the mode on as it runs, a shell applies this to
// its live parser between input lines rather than only at NewParser:
// bash reads a whole line before running any of it, so `set -o posix`
// takes effect on the *next* line, which is what applying it between
// lines reproduces.
func POSIXMode(enabled bool) ParserOption {
	return func(p *Parser) { p.posix = enabled }
}

// LangVariant describes a shell language variant to use when tokenizing and
// parsing shell code. The zero value is [LangBash].
//
// This type implements [flag.Value] so that it can be used as a CLI flag.
type LangVariant int

// TODO(v4): the zero value should be left as an unset and invalid value.
// TODO(v4): the type should be uint32 now that we use this as a bitset;
// an unsigned integer is clearer, and being agnostic to uint size avoids issues.

const (
	// LangBash corresponds to the GNU Bash language, as described in its
	// manual at https://www.gnu.org/software/bash/manual/bash.html.
	//
	// We currently follow Bash version 5.2.
	//
	// Its string representation is "bash".
	LangBash LangVariant = 1 << iota

	// LangPOSIX corresponds to the POSIX Shell language, as described at
	// https://pubs.opengroup.org/onlinepubs/9699919799/utilities/V3_chap02.html.
	//
	// Its string representation is "posix" or "sh".
	LangPOSIX

	// LangMirBSDKorn corresponds to the MirBSD Korn Shell, also known as
	// mksh, as described at http://www.mirbsd.org/htman/i386/man1/mksh.htm.
	// Note that it shares some features with Bash, due to the shared
	// ancestry that is ksh.
	//
	// We currently follow mksh version 59.
	//
	// Its string representation is "mksh".
	LangMirBSDKorn

	// LangBats corresponds to the Bash Automated Testing System language,
	// as described at https://github.com/bats-core/bats-core. Note that
	// it's just a small extension of the Bash language.
	//
	// Its string representation is "bats".
	LangBats

	// LangZsh corresponds to the Z shell, as described at https://www.zsh.org/.
	//
	// Note that its support in the syntax package is experimental and
	// incomplete for now. See https://github.com/mvdan/sh/issues/120.
	//
	// We currently follow Zsh version 5.9.
	//
	// Its string representation is "zsh".
	LangZsh

	// LangAuto corresponds to automatic language detection,
	// commonly used by end-user applications like shfmt,
	// which can guess a file's language variant given its filename or shebang.
	//
	// At this time, [Variant] does not support LangAuto.
	LangAuto

	// langBashLegacy is what [LangBash] used to be, when it was zero.
	// We still support it for the sake of backwards compatibility.
	langBashLegacy LangVariant = 0

	// langResolvedVariants contains all known variants except [LangAuto],
	// which is meant to resolve to another variant.
	langResolvedVariants = LangBash | LangPOSIX | LangMirBSDKorn | LangBats | LangZsh

	// langResolvedVariantsCount is langResolvedVariants.count() as a constant.
	// TODO: Can we compute this as a constant expression somehow?
	// For example, if we had log2, we could do log2(LangAuto).
	langResolvedVariantsCount = 5

	// langBashLike contains Bash plus all variants which are extensions of it.
	langBashLike = LangBash | LangBats
)

// Variant changes the shell language variant that the parser will
// accept.
//
// The passed language variant must be one of the constant values defined in
// this package.
func Variant(l LangVariant) ParserOption {
	switch l {
	case langBashLegacy:
		l = LangBash
	case LangBash, LangPOSIX, LangMirBSDKorn, LangBats, LangZsh:
	case LangAuto:
		panic("LangAuto is not supported by the parser at this time")
	default:
		panic(fmt.Sprintf("unknown shell language variant: %#b", l))
	}
	return func(p *Parser) { p.lang = l }
}

func (l LangVariant) String() string {
	switch l {
	case langBashLegacy, LangBash:
		return "bash"
	case LangPOSIX:
		return "posix"
	case LangMirBSDKorn:
		return "mksh"
	case LangBats:
		return "bats"
	case LangZsh:
		return "zsh"
	case LangAuto:
		return "auto"
	}
	return "unknown shell language variant"
}

func (l *LangVariant) Set(s string) error {
	switch s {
	case "bash":
		*l = LangBash
	case "posix", "sh", "dash":
		*l = LangPOSIX
	case "mksh":
		*l = LangMirBSDKorn
	case "bats":
		*l = LangBats
	case "zsh":
		*l = LangZsh
	case "auto":
		*l = LangAuto
	default:
		return fmt.Errorf("unknown shell language variant: %q", s)
	}
	return nil
}

func (l LangVariant) in(l2 LangVariant) bool {
	return l&l2 == l
}

func (l LangVariant) count() int {
	return bits.OnesCount32(uint32(l))
}

func (l LangVariant) index() int {
	return bits.TrailingZeros32(uint32(l))
}

func (l LangVariant) bits() iter.Seq[LangVariant] {
	return func(yield func(LangVariant) bool) {
		for n := LangVariant(1); n < langResolvedVariants; n <<= 1 {
			if l&n == 0 {
				continue
			}
			if !yield(n) {
				return
			}
		}
	}
}

// StopAt configures the lexer to stop at an arbitrary word, treating it
// as if it were the end of the input. It can contain any characters
// except whitespace, and cannot be over four bytes in size.
//
// This can be useful to embed shell code within another language, as
// one can use a special word to mark the delimiters between the two.
//
// As a word, it will only apply when following whitespace or a
// separating token. For example, StopAt("$$") will act on the inputs
// "foo $$" and "foo;$$", but not on "foo '$$'".
//
// The match is done by prefix, so the example above will also act on
// "foo $$bar".
func StopAt(word string) ParserOption {
	if len(word) > 4 {
		panic("stop word can't be over four bytes in size")
	}
	if strings.ContainsAny(word, " \t\n\r") {
		panic("stop word can't contain whitespace characters")
	}
	return func(p *Parser) { p.stopAt = []byte(word) }
}

// RecoverErrors allows the parser to skip up to a maximum number of
// errors in the given input on a best-effort basis.
// This can be useful to tab-complete an interactive shell prompt,
// or when providing diagnostics on slightly incomplete shell source.
//
// Currently, this only helps with mandatory tokens from the shell grammar
// which are not present in the input. They result in position fields
// or nodes whose position report [Pos.IsRecovered] as true.
//
// For example, given the input
//
//	(foo |
//
// the result will contain two recovered positions; first, the pipe requires
// a statement to follow, and as [Stmt.Pos] reports, the entire node is recovered.
// Second, the subshell needs to be closed, so [Subshell.Rparen] is recovered.
func RecoverErrors(maximum int) ParserOption {
	return func(p *Parser) { p.recoverErrorsMax = maximum }
}

// NewParser allocates a new [Parser] and applies any number of options.
func NewParser(options ...ParserOption) *Parser {
	p := &Parser{
		lang: LangBash,
	}
	for _, opt := range options {
		opt(p)
	}
	return p
}

// Parse reads and parses a shell program with an optional name. It
// returns the parsed program if no issues were encountered. Otherwise,
// an error is returned. Reads from r are buffered.
//
// Parse can be called more than once, but not concurrently. That is, a
// Parser can be reused once it is done working.
func (p *Parser) Parse(r io.Reader, name string) (*File, error) {
	p.reset()
	p.f = &File{Name: name}
	p.src = r
	p.rune()
	p.next()
	p.f.Stmts, p.f.Last = p.stmtList()
	if p.err == nil {
		// EOF immediately after heredoc word so no newline to
		// trigger the parsing error.
		p.doHeredocs()
	}
	if p.err == nil && len(p.recoverable) > 0 {
		// Whoever parses a whole input at once has no unit smaller than
		// that to discard, so a recoverable error is returned as an
		// ordinary one here (#581) — which is also right for the caller
		// that has exactly one unit, an interactive line. Only the
		// incremental readers ask [Parser.RecoverableErrors].
		return p.f, p.recoverable[0]
	}
	return p.f, p.err
}

// Stmts is a pre-iterators API which now wraps [Parser.StmtsSeq].
//
// Deprecated: use [Parser.StmtsSeq].
func (p *Parser) Stmts(r io.Reader, fn func(*Stmt) bool) error {
	for stmt, err := range p.StmtsSeq(r) {
		if err != nil {
			return err
		}
		if !fn(stmt) {
			break
		}
	}
	return nil
}

// AtLineEnd reports whether the parser has finished reading an input
// line: the token it stopped at is a newline or the end of the input.
//
// It is meant to be asked right after [Parser.StmtsSeq] yields, by a
// shell which reads and runs a line at a time. `echo a; echo b` yields
// twice with only the second at a line end, which is what makes the two
// commands one unit — bash reads the whole line before running any of
// it, so a statement can only change how the *next* line is read.
func (p *Parser) AtLineEnd() bool {
	return p.tok == _Newl || p.tok == _EOF
}

// StmtsSeq reads and parses statements one at a time via an iterator.
func (p *Parser) StmtsSeq(r io.Reader) iter.Seq2[*Stmt, error] {
	p.reset()
	p.f = &File{}
	p.src = r
	return func(yield func(*Stmt, error) bool) {
		p.rune()
		p.next()
		p.stmts(yield)
		if p.err == nil {
			// EOF immediately after heredoc word so no newline to
			// trigger the parsing error.
			p.doHeredocs()
		}
		if p.err != nil {
			// Yield any final error from the parser.
			yield(nil, p.err)
		}
	}
}

type wrappedReader struct {
	p  *Parser
	rd io.Reader

	lastLine    int64
	accumulated []*Stmt
	yield       func([]*Stmt, error) bool
}

func (w *wrappedReader) Read(p []byte) (n int, err error) {
	// If we lexed a newline for the first time, we just finished a line, so
	// we may need to give a callback for the edge cases below not covered
	// by [Parser.Stmts].
	if (w.p.r == '\n' || w.p.r == escNewl) && w.p.line > w.lastLine {
		if w.p.Incomplete() {
			// Incomplete statement; call back to print "> ".
			if !w.yield(w.accumulated, w.p.err) {
				return 0, io.EOF
			}
		} else if len(w.accumulated) == 0 {
			// Nothing was parsed; call back to print another "$ ".
			if !w.yield(nil, w.p.err) {
				return 0, io.EOF
			}
		}
		w.lastLine = w.p.line
	}
	return w.rd.Read(p)
}

// Interactive is a pre-iterators API which now wraps [Parser.InteractiveSeq].
//
// Deprecated: use [Parser.InteractiveSeq].
func (p *Parser) Interactive(r io.Reader, fn func([]*Stmt) bool) error {
	for stmts, err := range p.InteractiveSeq(r) {
		if err != nil {
			return err
		}
		if !fn(stmts) {
			break
		}
	}
	return nil
}

// InteractiveSeq implements what is necessary to parse statements in an
// interactive shell. The parser will call the given function under two
// circumstances outlined below.
//
// If a line containing any number of statements is parsed, the function will be
// called with said statements.
//
// If a line ending in an incomplete statement is parsed, the function will be
// called with any fully parsed statements, and [Parser.Incomplete] will return true.
//
// One can imagine a simple interactive shell implementation as follows:
//
//	fmt.Fprintf(os.Stdout, "$ ")
//	parser.Interactive(os.Stdin, func(stmts []*syntax.Stmt) bool {
//		if parser.Incomplete() {
//			fmt.Fprintf(os.Stdout, "> ")
//			return true
//		}
//		run(stmts)
//		fmt.Fprintf(os.Stdout, "$ ")
//		return true
//	}
//
// If the callback function returns false, parsing is stopped and the function
// is not called again.
func (p *Parser) InteractiveSeq(r io.Reader) iter.Seq2[[]*Stmt, error] {
	return func(yield func([]*Stmt, error) bool) {
		w := wrappedReader{p: p, rd: r, yield: yield}
		for stmts, err := range p.StmtsSeq(&w) {
			w.accumulated = append(w.accumulated, stmts)
			if err != nil {
				if !yield(w.accumulated, err) {
					break
				}
				// If the caller wishes, they can continue in the presence of parse errors.
				// TODO: does this even work? Write tests for it. This only came up
				continue
			}
			// We finished parsing a statement and we're at a newline token,
			// so we finished fully parsing a number of statements. Call
			// back to run the statements and print "$ ".
			if p.tok == _Newl {
				if !yield(w.accumulated, nil) {
					break
				}
				w.accumulated = w.accumulated[:0]
				// The callback above would already print "$ ", so we
				// don't want the subsequent wrappedReader.Read to cause
				// another "$ " print thinking that nothing was parsed.
				w.lastLine = w.p.line + 1
			}
		}
	}
}

// Words is a pre-iterators API which now wraps [Parser.WordsSeq].
//
// Deprecated: use [Parser.WordsSeq].
func (p *Parser) Words(r io.Reader, fn func(*Word) bool) error {
	for w, err := range p.WordsSeq(r) {
		if err != nil {
			return err
		}
		if !fn(w) {
			break
		}
	}
	return nil
}

// WordsSeq reads and parses a sequence of words alongside any error encountered.
//
// Newlines are skipped, meaning that multi-line input will work fine. If the
// parser encounters a token that isn't a word, such as a semicolon, an error
// will be returned.
//
// Note that the lexer doesn't currently tokenize spaces, so it may need to read
// a non-space byte such as a newline or a letter before finishing the parsing
// of a word. This will be fixed in the future.
func (p *Parser) WordsSeq(r io.Reader) iter.Seq2[*Word, error] {
	p.reset()
	p.f = &File{}
	p.src = r
	return func(yield func(*Word, error) bool) {
		p.rune()
		p.next()
		for {
			p.got(_Newl)
			w := p.getWord()
			if w == nil {
				if p.tok != _EOF {
					p.curErr("%#q is not a valid word", p.tok)
				}
				if p.err != nil {
					yield(nil, p.err)
				}
				return
			}
			if !yield(w, nil) {
				return
			}
		}
	}
}

// Document parses a single here-document word. That is, it parses the input as
// if they were lines following a <<EOF redirection.
//
// In practice, this is the same as parsing the input as if it were within
// double quotes, but without having to escape all double quote characters.
// Similarly, the here-document word parsed here cannot be ended by any
// delimiter other than reaching the end of the input.
func (p *Parser) Document(r io.Reader) (*Word, error) {
	p.reset()
	p.f = &File{}
	p.src = r
	p.rune()
	p.quote = hdocBody
	p.hdocStops = [][]byte{[]byte("MVDAN_CC_SH_SYNTAX_EOF")}
	p.parsingDoc = true
	p.next()
	w := p.getWord()
	return w, p.err
}

// Arithmetic parses a single arithmetic expression. That is, as if the input
// were within the $(( and )) tokens.
//
// The whole input has to be that one expression. Text left over after it is
// an error rather than a partial read, because a partial read is a silent
// wrong answer: `hello world` parses `hello`, stops, and evaluates the empty
// value of a name nobody set as zero, where bash answers `hello world:
// arithmetic syntax error in expression (error token is "world")` (#564).
// Every caller re-parses a *string* as arithmetic — a subscript, a nameref's
// subscript, a `[[ x -eq y ]]` operand, a name's value read arithmetically —
// so every one of them was answering zero for text bash refuses.
func (p *Parser) Arithmetic(r io.Reader) (ArithmExpr, error) {
	p.reset()
	p.f = &File{}
	p.src = r
	p.rune()
	p.quote = arithmExpr
	p.next()
	expr := p.arithmExpr(false)
	if p.err == nil && p.tok != _EOF {
		switch p.tok {
		case _Lit, _LitWord:
			p.curErr("not a valid arithmetic operator: %#q", p.val)
		default:
			p.curErr("not a valid arithmetic operator: %#q", p.tok)
		}
	}
	return expr, p.err
}

// Parser holds the internal state of the parsing mechanism of a
// program.
type Parser struct {
	src io.Reader
	bs  []byte // current chunk of read bytes
	bsp uint   // offset within [Parser.bs] for the rune after [Parser.r]
	r   rune   // next rune; [runeEOF] when it went past EOF, or we stopped
	w   int    // width of [Parser.r]

	f *File

	spaced bool // whether [Parser.tok] has whitespace on its left

	err     error // lexer/parser error
	readErr error // got a read error, but bytes left
	readEOF bool  // [Parser.src] already gave us an [io.EOF] error

	tok token  // current token
	val string // current value (valid if tok is _Lit*)

	// position of [Parser.r], to be converted to [Parser.pos] later
	offs, line, col int64

	// fragmentPos is where the input this parser reads sits inside a
	// larger source, for a sub-parser handed a piece the outer parser
	// has already lexed (#564). Every node then carries the position it
	// would have had if it had been parsed in place, which is what makes
	// a diagnostic about it point at the right column and what keeps
	// [Node.Pos] ordered across the tree.
	fragmentPos Pos

	pos Pos // position of tok

	quote   quoteState // current lexer state
	eqlOffs int        // position of '=' in [Parser.val] when [Parser.tok].isLit is true

	keepComments bool
	lang         LangVariant
	// posix is bash's `set -o posix`; see [POSIXMode].
	posix bool
	// sglQuoteLiteral is set while lexing the word of a `${name+word}`
	// expansion which posix mode says quotes are not special in. See
	// [POSIXMode] and [Parser.paramExpExp].
	sglQuoteLiteral bool

	stopAt []byte

	recoveredErrors  int
	recoverErrorsMax int

	// recoverable collects the errors bash discards an input unit for
	// rather than ending the shell — see [ParseError.Recoverable]. They
	// are not returned, because parsing continues past them.
	recoverable []ParseError

	forbidNested bool

	// list of pending heredoc bodies
	buriedHdocs int
	heredocs    []*Redirect

	hdocStops [][]byte // stack of end words for open heredocs

	parsingDoc bool // true if using [Parser.Document]

	// openNodes tracks how many entire statements or words we're currently parsing.
	// A non-zero number means that we require certain tokens or words before
	// reaching EOF, used for [Parser.Incomplete].
	openNodes int
	// openBquotes is how many levels of backquotes are open at the moment.
	openBquotes int
	// openBquoteDbls is how many of those backquote levels began inside
	// double quotes, where backslashes also escape double quotes.
	openBquoteDbls int

	// lastBquoteEsc is how many times the last backquote token was escaped
	lastBquoteEsc int

	rxOpenParens int
	rxFirstPart  bool

	accComs []Comment
	curComs *[]Comment

	litBatch  []Lit
	wordBatch []wordAlloc

	readBuf [bufSize]byte
	litBuf  [bufSize]byte
	litBs   []byte
}

// Incomplete reports whether the parser needs more input bytes
// to finish properly parsing a statement or word.
//
// It is only safe to call while the parser is blocked on a read. For an example
// use case, see [Parser.Interactive].
func (p *Parser) Incomplete() bool {
	// If there are any open nodes, we need to finish them.
	// If we're constructing a literal, we need to finish it.
	return p.openNodes > 0 || len(p.litBs) > 0
}

const bufSize = 1 << 10

func (p *Parser) reset() {
	p.tok, p.val = illegalTok, ""
	p.eqlOffs = 0
	p.bs, p.bsp = nil, 0
	p.offs, p.line, p.col = 0, 1, 1
	if p.fragmentPos.IsValid() {
		p.offs = int64(p.fragmentPos.Offset())
		p.line = int64(p.fragmentPos.Line())
		p.col = int64(p.fragmentPos.Col())
	}
	p.r, p.w = 0, 0
	p.err, p.readErr, p.readEOF = nil, nil, false
	p.quote, p.forbidNested = noState, false
	p.openNodes = 0
	p.recoveredErrors = 0
	p.recoverable = nil
	p.heredocs, p.buriedHdocs = p.heredocs[:0], 0
	p.hdocStops = nil
	p.parsingDoc = false
	p.openBquotes = 0
	p.openBquoteDbls = 0
	p.accComs = nil
	p.accComs, p.curComs = nil, &p.accComs
	p.litBatch = nil
	p.wordBatch = nil
	p.litBs = nil
}

// nextPos returns the position of the next rune, [Parser.r].
func (p *Parser) nextPos() Pos {
	// Basic protection against offset overflow;
	// note that an offset of 0 is valid, so we leave the maximum.
	offset := min(p.offs+int64(p.bsp)-int64(p.w), offsetMax)
	var line, col uint
	if p.line <= lineMax {
		line = uint(p.line)
	}
	if p.col <= colMax {
		col = uint(p.col)
	}
	return NewPos(uint(offset), line, col)
}

func (p *Parser) lit(pos Pos, val string) *Lit {
	if len(p.litBatch) == 0 {
		p.litBatch = make([]Lit, 32)
	}
	l := &p.litBatch[0]
	p.litBatch = p.litBatch[1:]
	l.ValuePos = pos
	l.ValueEnd = p.nextPos()
	l.Value = val
	return l
}

type wordAlloc struct {
	word  Word
	parts [1]WordPart
}

func (p *Parser) wordAnyNumber() *Word {
	if len(p.wordBatch) == 0 {
		p.wordBatch = make([]wordAlloc, 32)
	}
	alloc := &p.wordBatch[0]
	p.wordBatch = p.wordBatch[1:]
	w := &alloc.word
	w.Parts = p.wordParts(alloc.parts[:0])
	return w
}

func (p *Parser) wordOne(part WordPart) *Word {
	if len(p.wordBatch) == 0 {
		p.wordBatch = make([]wordAlloc, 32)
	}
	alloc := &p.wordBatch[0]
	p.wordBatch = p.wordBatch[1:]
	w := &alloc.word
	w.Parts = alloc.parts[:1]
	w.Parts[0] = part
	return w
}

func (p *Parser) call(w *Word) *CallExpr {
	var alloc struct {
		ce CallExpr
		ws [4]*Word
	}
	ce := &alloc.ce
	ce.Args = alloc.ws[:1]
	ce.Args[0] = w
	return ce
}

type quoteState uint32

const (
	// The initial state of the parser.
	noState quoteState = 1 << iota

	// Used when parsing parameter expansions; use with [Parser.rune],
	// [Parser.next] always returns [illegalTok].
	runeByRune

	// unquotedWordCont exists purely so that the '#' in $foo#bar does not
	// get parsed as a comment; it's a tiny variation on [noState].
	unquotedWordCont

	subCmd
	subCmdBckquo
	// subCmdBraces is like subCmd, but for `${ stmts;}` and `${|stmts;}`,
	// whose bodies end at a word beginning with `}`.
	subCmdBraces
	dblQuotes
	hdocWord
	hdocBody
	hdocBodyTabs
	arithmExpr
	arithmExprLet
	arithmExprCmd
	testExpr
	testExprRegexp
	switchCase
	paramExpArithm
	paramExpRepl
	paramExpExp
	arrayElems
	// subscriptWord lexes the *text* of a subscript that is not an
	// arithmetic expression — `m[hello world]` — where every character
	// up to the matching `]` is ordinary but quotes and expansions are
	// still special. It is paramExpExp without `}` ending the word,
	// which is why it cannot simply reuse it: a subscript is scanned to
	// its bracket, so a brace in it is a brace (#564).
	subscriptWord

	allKeepSpaces = runeByRune | paramExpRepl | dblQuotes | hdocBody |
		hdocBodyTabs | paramExpRepl | paramExpExp | subscriptWord
	// allWholeText is where a `/` is an ordinary character rather than
	// the start of a replacement.
	allWholeText = paramExpExp | subscriptWord
	allRegTokens = noState | unquotedWordCont | subCmd | subCmdBckquo | subCmdBraces |
		hdocWord | switchCase | arrayElems | testExpr
	allArithmExpr = arithmExpr | arithmExprLet | arithmExprCmd | paramExpArithm
	allParamExp   = paramExpArithm | paramExpRepl | paramExpExp
)

type saveState struct {
	quote       quoteState
	buriedHdocs int
}

func (p *Parser) preNested(quote quoteState) (s saveState) {
	s.quote, s.buriedHdocs = p.quote, p.buriedHdocs
	p.buriedHdocs, p.quote = len(p.heredocs), quote
	return s
}

func (p *Parser) postNested(s saveState) {
	p.quote, p.buriedHdocs = s.quote, s.buriedHdocs
}

func (p *Parser) unquotedWordBytes(w *Word) ([]byte, bool) {
	buf := make([]byte, 0, 4)
	didUnquote := false
	for _, wp := range w.Parts {
		buf, didUnquote = p.unquotedWordPart(buf, wp, false)
	}
	return buf, didUnquote
}

func (p *Parser) unquotedWordPart(buf []byte, wp WordPart, quotes bool) (_ []byte, quoted bool) {
	switch wp := wp.(type) {
	case *Lit:
		for i := 0; i < len(wp.Value); i++ {
			if b := wp.Value[i]; b == '\\' && !quotes {
				if i++; i < len(wp.Value) {
					buf = append(buf, wp.Value[i])
				}
				quoted = true
			} else {
				buf = append(buf, b)
			}
		}
	case *SglQuoted:
		buf = append(buf, []byte(wp.Value)...)
		quoted = true
	case *DblQuoted:
		for _, wp2 := range wp.Parts {
			buf, _ = p.unquotedWordPart(buf, wp2, true)
		}
		quoted = true
	}
	return buf, quoted
}

func (p *Parser) doHeredocs() {
	hdocs := p.heredocs[p.buriedHdocs:]
	if len(hdocs) == 0 {
		// Nothing do do; don't even issue a read.
		return
	}
	p.rune() // consume '\n', since we know p.tok == _Newl
	old := p.quote
	p.heredocs = p.heredocs[:p.buriedHdocs]
	for i, r := range hdocs {
		if p.err != nil {
			break
		}
		p.quote = hdocBody
		if r.Op == DashHdoc {
			p.quote = hdocBodyTabs
		}
		stop, quoted := p.unquotedWordBytes(r.Word)
		p.hdocStops = append(p.hdocStops, stop)
		if i > 0 && p.r == '\n' {
			p.rune()
		}
		if quoted {
			r.Hdoc = p.quotedHdocWord()
		} else {
			p.next()
			r.Hdoc = p.getWord()
		}
		if stop := p.hdocStops[len(p.hdocStops)-1]; stop != nil {
			p.posErr(r.Pos(), "unclosed here-document %#q", stop)
		}
		p.hdocStops = p.hdocStops[:len(p.hdocStops)-1]
	}
	p.quote = old
}

func (p *Parser) got(tok token) bool {
	if p.tok == tok {
		p.next()
		return true
	}
	return false
}

func (p *Parser) gotRsrv(val string) (Pos, bool) {
	pos := p.pos
	if p.tok == _LitWord && p.val == val {
		p.next()
		return pos, true
	}
	return pos, false
}

func (p *Parser) recoverError() bool {
	if p.recoveredErrors < p.recoverErrorsMax {
		p.recoveredErrors++
		return true
	}
	return false
}

type noQuote string

func (s noQuote) Format(f fmt.State, verb rune) {
	f.Write([]byte(s))
}

func (t token) Format(f fmt.State, verb rune) {
	if t < _realTokenBoundary && verb == 'q' {
		// EOF, Lit and the others should not be quoted in error messages
		// as they are not real shell syntax like `if` or `{`.
		f.Write([]byte(t.String()))
	} else {
		fmt.Fprintf(f, fmt.FormatString(f, verb), t.String())
	}
}

func (p *Parser) followErr(pos Pos, left, right any) {
	p.posErr(pos, "%#q must be followed by %#q", left, right)
}

func (p *Parser) followErrExp(pos Pos, left any) {
	p.followErr(pos, left, noQuote("an expression"))
}

func (p *Parser) follow(lpos Pos, left string, tok token) {
	if !p.got(tok) {
		p.followErr(lpos, left, tok)
	}
}

func (p *Parser) followRsrv(lpos Pos, left, val string) Pos {
	pos, ok := p.gotRsrv(val)
	if !ok {
		if p.recoverError() {
			return recoveredPos
		}
		p.followErr(lpos, left, val)
	}
	return pos
}

func (p *Parser) followStmts(left string, lpos Pos, stops ...string) ([]*Stmt, []Comment) {
	// Language variants disallowing empty command lists:
	// * [LangPOSIX]: "A list is a sequence of one or more AND-OR lists...".
	// * [LangBash]: "A list is a sequence of one or more pipelines..."
	//
	// Language variants allowing empty command lists:
	// * [LangZsh]: "A list is a sequence of zero or more sublists...".
	// * [LangMirBSDKorn]: "Lists of commands can be created by separating pipelines...";
	//   note that the man page is not explicit, but the shell clearly allows e.g. `{ }`.
	if p.got(semicolon) {
		if p.lang.in(LangZsh | LangMirBSDKorn) {
			return nil, nil // allow an empty list
		}
		p.followErr(lpos, left, noQuote("a statement list"))
		return nil, nil
	}
	stmts, last := p.stmtList(stops...)
	if len(stmts) < 1 {
		if p.lang.in(LangZsh | LangMirBSDKorn) {
			return nil, nil // allow an empty list
		}
		if p.recoverError() {
			return []*Stmt{{Position: recoveredPos}}, nil
		}
		p.followErr(lpos, left, noQuote("a statement list"))
	}
	return stmts, last
}

func (p *Parser) followWordTok(tok token, pos Pos) *Word {
	w := p.getWord()
	if w == nil {
		if p.recoverError() {
			return p.wordOne(&Lit{ValuePos: recoveredPos})
		}
		p.followErr(pos, tok, noQuote("a word"))
	}
	return w
}

func (p *Parser) stmtEnd(n Node, start, end string) Pos {
	pos, ok := p.gotRsrv(end)
	if !ok {
		if p.recoverError() {
			return recoveredPos
		}
		p.posErr(n.Pos(), "%#q statement must end with %#q", start, end)
	}
	return pos
}

func (p *Parser) quoteErr(lpos Pos, quote token) {
	p.posErr(lpos, "reached %#q without closing quote %#q", p.tok, quote)
}

func (p *Parser) matchingErr(lpos Pos, left, right token) {
	p.posErr(lpos, "reached %#q without matching %#q with %#q", p.tok, left, right)
}

func (p *Parser) matched(lpos Pos, left, right token) Pos {
	pos := p.pos
	if !p.got(right) {
		if p.recoverError() {
			return recoveredPos
		}
		p.matchingErr(lpos, left, right)
	}
	return pos
}

func (p *Parser) errPass(err error) {
	if p.err == nil {
		p.err = err
		p.bsp = uint(len(p.bs)) + 1
		p.r = runeEOF
		p.w = 1
		p.tok = _EOF
	}
}

// IsIncomplete reports whether a Parser error could have been avoided with
// extra input bytes. For example, if an [io.EOF] was encountered while there was
// an unclosed quote or parenthesis.
func IsIncomplete(err error) bool {
	perr, ok := err.(ParseError)
	return ok && perr.Incomplete
}

// TODO: probably redo with a [LangVariant] argument.
// Perhaps offer an iterator version as well.

// IsKeyword returns true if the given word is a language keyword
// in POSIX Shell or Bash.
func IsKeyword(word string) bool {
	// This list has been copied from the bash 5.1 source code, file y.tab.c +4460
	// TODO: should we include entries for zsh here? e.g. "{}", "repeat", "always", ...
	switch word {
	case
		"!",
		"[[", // only if COND_COMMAND is defined
		"]]", // only if COND_COMMAND is defined
		"case",
		"coproc", // only if COPROCESS_SUPPORT is defined
		"do",
		"done",
		"else",
		"esac",
		"fi",
		"for",
		"function",
		"if",
		"in",
		"select", // only if SELECT_COMMAND is defined
		"then",
		"time", // only if COMMAND_TIMING is defined
		"until",
		"while",
		"{",
		"}":
		return true
	}
	return false
}

// ParseError represents an error found when parsing a source file, from which
// the parser cannot recover.
type ParseError struct {
	Filename string
	Pos      Pos
	Text     string

	Incomplete bool

	// Recoverable marks an error bash reports and then carries on from:
	// the input *unit* being parsed is discarded — the line of a script,
	// the whole string of a `-c` — and reading continues after it with a
	// status of 1 (#581). An unexpected token inside a compound array
	// assignment is the case; an ordinary grammar error is not, and bash
	// ends a non-interactive shell for those.
	//
	// Errors like this are not returned; they are collected and handed
	// over by [Parser.RecoverableErrors], since the parser keeps going.
	// Unrelated to the [RecoverErrors] option, which invents grammar
	// tokens the input is missing.
	Recoverable bool
}

func (e ParseError) Error() string {
	if e.Filename == "" {
		return fmt.Sprintf("%s: %s", e.Pos, e.Text)
	}
	return fmt.Sprintf("%s:%s: %s", e.Filename, e.Pos, e.Text)
}

// LangError is returned when the parser encounters code that is only valid in
// other shell language variants. The error includes what feature is not present
// in the current language variant, and what languages support it.
type LangError struct {
	Filename string
	Pos      Pos

	// TODO: consider replacing the Langs slice with a bitset.

	// Feature briefly describes which language feature caused the error.
	Feature string
	// Langs lists some of the language variants which support the feature.
	Langs []LangVariant
	// LangUsed is the language variant used which led to the error.
	LangUsed LangVariant
}

func (e LangError) Error() string {
	var sb strings.Builder
	if e.Filename != "" {
		sb.WriteString(e.Filename)
		sb.WriteString(":")
	}
	sb.WriteString(e.Pos.String())
	sb.WriteString(": ")
	sb.WriteString(e.Feature)
	if strings.HasSuffix(e.Feature, "s") {
		sb.WriteString(" are a ")
	} else {
		sb.WriteString(" is a ")
	}
	for i, lang := range e.Langs {
		if i > 0 {
			sb.WriteString("/")
		}
		sb.WriteString(lang.String())
	}
	sb.WriteString(" feature; tried parsing as ")
	sb.WriteString(e.LangUsed.String())
	return sb.String()
}

func (p *Parser) posErr(pos Pos, format string, args ...any) {
	// for i, arg := range args {
	// 	if arg, ok := arg.(fmt.Stringer); ok && arg != _EOF {
	// 		args[i] = quotedToken(arg)
	// 	}
	// }
	p.errPass(ParseError{
		Filename:   p.f.Name,
		Pos:        pos,
		Text:       fmt.Sprintf(format, args...),
		Incomplete: p.tok == _EOF && p.Incomplete(),
	})
}

func (p *Parser) curErr(format string, args ...any) {
	p.posErr(p.pos, format, args...)
}

// recoverableErr records an error that costs its input unit and lets
// parsing carry on, which is what bash does with a syntax error inside a
// compound array assignment (#581). See [ParseError.Recoverable].
func (p *Parser) recoverableErr(pos Pos, format string, args ...any) {
	p.recoverable = append(p.recoverable, ParseError{
		Filename:    p.f.Name,
		Pos:         pos,
		Text:        fmt.Sprintf(format, args...),
		Recoverable: true,
	})
}

// RecoverableErrors returns the errors found so far which cost their
// input unit without stopping the parse, and forgets them.
//
// A caller reading input incrementally asks after each unit: statements
// from a unit with one of these must not run, and reading continues. See
// [ParseError.Recoverable].
func (p *Parser) RecoverableErrors() []ParseError {
	errs := p.recoverable
	p.recoverable = nil
	return errs
}

func (p *Parser) checkLang(pos Pos, langSet LangVariant, format string, a ...any) {
	if p.lang.in(langSet) {
		return
	}
	if langBashLike.in(langSet) {
		// If we're reporting an error because a feature is for bash-like funcs,
		// just mention "bash" rather than "bash/bats" for the sake of clarity.
		langSet &^= LangBats
	}
	p.errPass(LangError{
		Filename: p.f.Name,
		Pos:      pos,
		Feature:  fmt.Sprintf(format, a...),
		Langs:    slices.Collect(langSet.bits()),
		LangUsed: p.lang,
	})
}

func (p *Parser) stmts(yield func(*Stmt, error) bool, stops ...string) {
	gotEnd := true
loop:
	for p.tok != _EOF {
		newLine := p.got(_Newl)
		switch p.tok {
		case _LitWord:
			for _, stop := range stops {
				if p.val == stop {
					break loop
				}
			}
			if p.val == "}" {
				p.curErr(`%#q can only be used to close a block`, rightBrace)
			}
		case rightParen:
			if p.quote == subCmd {
				break loop
			}
		case bckQuote:
			if p.backquoteEnd() {
				break loop
			}
		case dblSemicolon, semiAnd, dblSemiAnd, semiOr:
			if p.quote == switchCase {
				break loop
			}
			p.curErr("%#q can only be used in a case clause", p.tok)
		}
		if !newLine && !gotEnd {
			p.curErr("statements must be separated by &, ; or a newline")
		}
		if p.tok == _EOF {
			break
		}
		p.openNodes++
		s := p.getStmt(true, false, false)
		p.openNodes--
		if s == nil {
			p.invalidStmtStart()
			break
		}
		gotEnd = s.Semicolon.IsValid()
		if !yield(s, p.err) {
			break
		}
	}
}

func (p *Parser) stmtList(stops ...string) ([]*Stmt, []Comment) {
	var stmts []*Stmt
	var last []Comment
	fn := func(s *Stmt, err error) bool {
		stmts = append(stmts, s)
		return true
	}
	p.stmts(fn, stops...)
	split := len(p.accComs)
	if p.tok == _LitWord && (p.val == "elif" || p.val == "else" || p.val == "fi") {
		// Split the comments, so that any aligned with an opening token
		// get attached to it. For example:
		//
		//     if foo; then
		//         # inside the body
		//     # document the else
		//     else
		//     fi
		// TODO(mvdan): look into deduplicating this with similar logic
		// in caseItems.
		for i, c := range slices.Backward(p.accComs) {
			if c.Pos().Col() != p.pos.Col() {
				break
			}
			split = i
		}
	}
	if split > 0 { // keep last nil if empty
		last = p.accComs[:split]
	}
	p.accComs = p.accComs[split:]
	return stmts, last
}

func (p *Parser) invalidStmtStart() {
	switch p.tok {
	case semicolon, and, or, andAnd, orOr, andPipe, andBang:
		p.curErr("%#q can only immediately follow a statement", p.tok)
	case rightParen:
		p.curErr("%#q can only be used to close a subshell", p.tok)
	default:
		p.curErr("%#q is not a valid start for a statement", p.tok)
	}
}

func (p *Parser) getWord() *Word {
	if w := p.wordAnyNumber(); len(w.Parts) > 0 && p.err == nil {
		return w
	}
	return nil
}

func (p *Parser) getLit() *Lit {
	if p.tok.isLit() {
		l := p.lit(p.pos, p.val)
		p.next()
		return l
	}
	return nil
}

func (p *Parser) wordParts(wps []WordPart) []WordPart {
	if p.quote == noState {
		p.quote = unquotedWordCont
		defer func() { p.quote = noState }()
	}
	for {
		p.openNodes++
		n := p.wordPart()
		p.openNodes--
		if n == nil {
			if len(wps) == 0 {
				return nil // normalize empty lists into nil
			}
			return wps
		}
		wps = append(wps, n)
		if p.spaced {
			return wps
		}
	}
}

func (p *Parser) ensureNoNested(pos Pos) {
	if p.forbidNested {
		p.posErr(pos, "expansions not allowed in heredoc words")
	}
}

func (p *Parser) wordPart() WordPart {
	switch p.tok {
	case _Lit, _LitWord, _LitRedir:
		l := p.lit(p.pos, p.val)
		p.next()
		return l
	case dollBrace:
		p.ensureNoNested(p.pos)
		switch p.r {
		case '|':
			p.checkLang(p.pos, langBashLike|LangMirBSDKorn, "`${|stmts;}`")
			fallthrough
		case ' ', '\t', '\n':
			p.checkLang(p.pos, langBashLike|LangMirBSDKorn, "`${ stmts;}`")
			cs := &CmdSubst{
				Left:     p.pos,
				TempFile: p.r != '|',
				ReplyVar: p.r == '|',
			}
			old := p.preNested(subCmdBraces)
			p.rune() // don't tokenize '|'
			p.next()
			cs.Stmts, cs.Last = p.stmtList("}")
			p.postNested(old)
			pos, ok := p.gotRsrv("}")
			if !ok {
				p.matchingErr(cs.Left, dollBrace, rightBrace)
			}
			cs.Right = pos
			return cs
		default:
			return p.paramExp()
		}
	case dollDblParen, dollBrack:
		p.ensureNoNested(p.pos)
		left := p.tok
		ar := &ArithmExp{Left: p.pos, Bracket: left == dollBrack}
		old := p.preNested(arithmExpr)
		p.next()
		if p.got(hash) {
			p.checkLang(ar.Pos(), LangMirBSDKorn, "unsigned expressions")
			ar.Unsigned = true
		}
		// An empty arithmetic expansion is valid and answers zero:
		// `$(())`, `$(( ))` and `$[]` are all 0 in bash, where koi
		// refused them as "must be followed by an expression".
		if p.peekArithmEnd() || (left == dollBrack && p.tok == rightBrack) {
			ar.X = nil
		} else {
			ar.X = p.followArithm(left, ar.Left)
		}
		if ar.Bracket {
			if p.tok != rightBrack {
				if p.recoverError() {
					ar.Right = recoveredPos
					return ar
				}
				p.arithmMatchingErr(ar.Left, dollBrack, rightBrack)
			}
			p.postNested(old)
			ar.Right = p.pos
			p.next()
		} else {
			ar.Right = p.arithmEnd(dollDblParen, ar.Left, old)
		}
		return ar
	case dollParen:
		p.ensureNoNested(p.pos)
		return p.cmdSubst()
	case dollar:
		pe := p.paramExp()
		if pe == nil { // was not actually a parameter expansion, like: "foo$"
			l := p.lit(p.pos, "$")
			p.next()
			return l
		}
		p.ensureNoNested(pe.Dollar)
		return pe
	case assgnParen:
		p.checkLang(p.pos, LangZsh, `%#q process substitutions`, p.tok)
		fallthrough
	case cmdIn, cmdOut:
		p.ensureNoNested(p.pos)
		ps := &ProcSubst{Op: ProcOperator(p.tok), OpPos: p.pos}
		old := p.preNested(subCmd)
		p.next()
		ps.Stmts, ps.Last = p.stmtList()
		p.postNested(old)
		ps.Rparen = p.matched(ps.OpPos, token(ps.Op), rightParen)
		return ps
	case sglQuote, dollSglQuote:
		sq := &SglQuoted{Left: p.pos, Dollar: p.tok == dollSglQuote}
		r := p.r
		for p.newLit(r); ; r = p.rune() {
			switch r {
			case '\\':
				if sq.Dollar {
					p.rune()
				}
			case '\'':
				sq.Right = p.nextPos()
				sq.Value = p.endLit()

				p.rune()
				p.next()
				return sq
			case escNewl:
				if p.openBquotes > 0 {
					// Inside backquotes, backslash-newline is removed
					// by the backquote-level scan before the inner text
					// is parsed — even inside single quotes, which are
					// otherwise literal (#423). Measured: `echo 'foo\
					// bar'` answers foobar, while the same single-quoted
					// string outside backquotes keeps both characters.
					continue
				}
				p.litBs = append(p.litBs, '\\', '\n')
			case runeEOF:
				p.tok = _EOF
				if p.recoverError() {
					sq.Right = recoveredPos
					return sq
				}
				p.quoteErr(sq.Pos(), sglQuote)
				return nil
			}
		}
	case dblQuote, dollDblQuote:
		if p.quote == dblQuotes {
			// p.tok == dblQuote, as "foo$" puts $ in the lit
			return nil
		}
		return p.dblQuoted()
	case bckQuote:
		if p.backquoteEnd() {
			return nil
		}
		p.ensureNoNested(p.pos)
		cs := &CmdSubst{Left: p.pos, Backquotes: true}
		old := p.preNested(subCmdBckquo)
		p.openBquotes++
		if old.quote == dblQuotes {
			p.openBquoteDbls++
		}

		// The lexer didn't call p.rune for us, so that it could have
		// the right p.openBquotes to properly handle backslashes.
		p.rune()

		p.next()
		cs.Stmts, cs.Last = p.stmtList()
		if p.tok == bckQuote && p.lastBquoteEsc < p.openBquotes-1 {
			// e.g. found ` before the nested backquote \` was closed.
			p.tok = _EOF
			p.quoteErr(cs.Pos(), bckQuote)
		}
		p.postNested(old)
		p.openBquotes--
		if old.quote == dblQuotes {
			p.openBquoteDbls--
		}
		cs.Right = p.pos

		// Like above, the lexer didn't call p.rune for us.
		p.rune()
		if !p.got(bckQuote) {
			if p.recoverError() {
				cs.Right = recoveredPos
			} else {
				p.quoteErr(cs.Pos(), bckQuote)
			}
		}
		return cs
	case leftParen:
		if p.lang.in(LangZsh) && p.r != ')' {
			// Zsh glob qualifier like *(N) or .(:a); the only case where
			// ( immediately after a word is not a glob qualifier is ()
			// for a function declaration, which the parser handles earlier.
			pos := p.pos
			p.pos = p.nextPos()
			for p.newLit(p.r); p.r != runeEOF && p.r != ')'; p.rune() {
			}
			if p.r != ')' {
				p.tok = _EOF // we can only get here due to EOF
				p.matchingErr(pos, leftParen, rightParen)
			}
			p.rune()
			p.val = p.endLit()
			l := p.lit(pos, "("+p.val)
			p.next()
			return l
		}
		return nil
	case globQuest, globStar, globPlus, globAt, globExcl:
		p.checkLang(p.pos, langBashLike|LangMirBSDKorn, "extended globs")
		eg := &ExtGlob{Op: GlobOperator(p.tok), OpPos: p.pos}
		lparens := 1
		r := p.r
	globLoop:
		for p.newLit(r); ; r = p.rune() {
			switch r {
			case runeEOF:
				break globLoop
			case '(':
				lparens++
			case ')':
				if lparens--; lparens == 0 {
					break globLoop
				}
			}
		}
		eg.Pattern = p.lit(posAddCol(eg.OpPos, 2), p.endLit())
		p.rune()
		p.next()
		if lparens != 0 {
			p.matchingErr(eg.OpPos, token(eg.Op), rightParen)
		}
		return eg
	default:
		return nil
	}
}

func (p *Parser) cmdSubst() *CmdSubst {
	cs := &CmdSubst{Left: p.pos}
	old := p.preNested(subCmd)
	p.next()
	cs.Stmts, cs.Last = p.stmtList()
	p.postNested(old)
	cs.Right = p.matched(cs.Left, dollParen, rightParen)
	return cs
}

func (p *Parser) dblQuoted() *DblQuoted {
	alloc := &struct {
		quoted DblQuoted
		parts  [1]WordPart
	}{
		quoted: DblQuoted{Left: p.pos, Dollar: p.tok == dollDblQuote},
	}
	q := &alloc.quoted
	old := p.quote
	p.quote = dblQuotes
	p.next()
	q.Parts = p.wordParts(alloc.parts[:0])
	p.quote = old
	q.Right = p.pos
	if !p.got(dblQuote) {
		if p.recoverError() {
			q.Right = recoveredPos
		} else {
			p.quoteErr(q.Pos(), dblQuote)
		}
	}
	return q
}

// paramExp parses a short or full parameter expansion, depending on whether
// [Parser.tok] is [dollar] or [dollBrace]. It returns nil if a [dollar] token
// does not form a valid parameter expansion, in which case it should be parsed
// as a literal.
func (p *Parser) paramExp() *ParamExp {
	old := p.quote
	p.quote = runeByRune
	// [ParamExp.Short] means we are parsing $exp rather than ${exp}.
	pe := &ParamExp{
		Dollar: p.pos,
		Short:  p.tok == dollar,
	}
	if !pe.Short && p.r == '(' {
		p.checkLang(pe.Pos(), LangZsh, `parameter expansion flags`)
		// For now, for simplicity, we parse flags as just a literal.
		// In the future, parsing as a word is better for cases like
		// `${(ps.$sep.)val}`.
		lparen := p.nextPos()
		p.rune()
		p.pos = p.nextPos()
		for p.newLit(p.r); p.r != runeEOF && p.r != ')'; p.rune() {
		}
		p.val = p.endLit()
		if p.r != ')' {
			p.tok = _EOF // we can only get here due to EOF
			p.matchingErr(lparen, leftParen, rightParen)
		}
		pe.Flags = p.lit(p.pos, p.val)
		p.rune()
	}
	// Zsh-only prefixes that change how the parameter is expanded.
	// They may appear in any combination, like ${=^name}.
	// Doubling the rune (${==a}, ${~~a}, ${^^a}) forces the option off.
zshPrefixLoop:
	for p.lang.in(LangZsh) {
		var field *OptState
		switch p.r {
		case '=':
			field = &pe.Split
		case '~':
			field = &pe.GlobSubst
		case '^':
			field = &pe.RcExpand
		default:
			break zshPrefixLoop
		}
		next, after := p.peekTwo()
		state := OptOn
		check := next
		if rune(next) == p.r {
			state = OptOff
			check = after
		}
		if check == utf8.RuneSelf || check == '}' {
			break zshPrefixLoop
		}
		// For the short form, only treat as a prefix if followed by something
		// that could start a parameter name or another zsh prefix.
		if pe.Short && check != '=' && check != '~' && check != '^' &&
			!singleRuneParam(check) && !paramNameRune(check) && check != '"' {
			break zshPrefixLoop
		}
		if state == OptOff {
			p.rune() // consume the first of the doubled pair
		}
		p.rune()
		*field = state
	}
	if !pe.Short || p.lang.in(LangZsh) {
		// Prefixes, like ${#name} to get the length of a variable.
		// Note that in Zsh, the short form like $#name is allowed too.
		switch p.r {
		case '#':
			if p.paramNameStart() {
				pe.Length = true
			}
		case '%':
			if p.paramNameStart() {
				p.checkLang(pe.Pos(), LangMirBSDKorn, "`${%%foo}`")
				pe.Width = true
			}
		case '!':
			// Unlike the others, zsh has no $!foo prefix.
			//
			// A `-` after it is the *operator* rather than the name of
			// the parameter to indirect through, so `${!-word}` is `$!`
			// with a default — which is what bash answers, and what
			// more-exp.tests stopped on (#277). `$-` is a parameter, so
			// without this exception it reads as one and the word after
			// it has nowhere to go.
			if !pe.Short && p.peek() != '-' && p.paramNameStart() {
				p.checkLang(pe.Pos(), langBashLike|LangMirBSDKorn, "`${!foo}`")
				pe.Excl = true
			}
		case '+':
			if p.paramNameStart() {
				p.checkLang(pe.Pos(), LangZsh, "`${+foo}`")
				pe.IsSet = true
			}
		}
	}
	if pe = p.paramExpParameter(pe); pe == nil {
		p.quote = old
		return nil // just "$"
	}
	// In short mode, any indexing or suffixes is not allowed, and we don't require '}'.
	// Zsh is an exception: $foo[1] and $foo[1,3] are valid. Note that $1[x] does not qualify.
	if pe.Short {
		if p.lang.in(LangZsh) && p.r == '[' && (len(p.val) != 1 || !positionalRuneParam(p.val[0])) {
			p.pos = p.nextPos()
			p.rune()
			pe.Index, _ = p.eitherIndex(false)
		}
		p.quote = old
		p.next()
		return pe
	}
	// Index expressions like ${foo[1]}. Note that expansion suffixes can be combined,
	// like ${foo[@]//replace/with}.
	if p.r == '[' {
		p.checkLang(p.nextPos(), langBashLike|LangMirBSDKorn|LangZsh, "arrays")
		// In zsh some of these like ${@[-1]} or ${*[1,3]} work,
		// so we don't do this sort of check at all.
		if !p.lang.in(LangZsh) && pe.Param != nil && !ValidName(pe.Param.Value) {
			// Taken before the scan, which moves the lexer to the closing
			// brace or to the end of the input.
			brackPos := p.nextPos()
			if bad := p.badParamExp(pe, old, brackPos, ""); bad != nil {
				return bad
			}
			p.posErr(brackPos, "cannot index a special parameter name")
		}
		p.pos = p.nextPos()
		p.rune()
		// `${b[]}` is bash's "bad substitution" at runtime and koi has
		// nowhere to hang that yet, so it stays a parse error (#582):
		// only an assignment target defers the empty case.
		pe.Index, _ = p.eitherIndex(false)
	}
	tokRune := p.r
	p.pos = p.nextPos()
	p.tok = p.paramToken(p.r)
	if p.tok == rightBrace {
		pe.Rbrace = p.pos
		p.quote = old
		p.next()
		return pe
	}
	if p.tok != _EOF && (pe.Length || pe.Width || pe.IsSet) {
		if bad := p.badParamExp(pe, old, p.pos, p.tok.String()); bad != nil {
			return bad
		}
		p.curErr("cannot combine multiple parameter expansion operators")
	}
	switch p.tok {
	case slash, dblSlash: // pattern search and replace
		p.checkLang(p.pos, langBashLike|LangMirBSDKorn|LangZsh, "search and replace")
		pe.Repl = &Replace{All: p.tok == dblSlash}
		p.quote = paramExpRepl
		p.next()
		pe.Repl.Orig = p.getWord()
		p.quote = paramExpExp
		if p.got(slash) {
			pe.Repl.With = p.getWord()
		}
	case colon: // slicing
		if p.lang.in(LangZsh) && (p.r == '&' || asciiLetter(p.r)) {
			pos := p.pos
		loop:
			for p.newLit(p.r); ; p.rune() {
				switch p.r {
				case runeEOF:
					p.tok = _EOF
					p.matchingErr(pe.Dollar, dollBrace, rightBrace)
					break loop
				case '}':
					pe.Modifiers = append(pe.Modifiers, p.lit(pos, p.endLit()))
					pe.Rbrace = p.nextPos()
					p.rune()
					break loop
				case ':':
					pe.Modifiers = append(pe.Modifiers, p.lit(pos, p.endLit()))
					p.rune()
					pos = p.nextPos()
					p.newLit(p.r)
				}
			}
			p.quote = old
			p.next()
			return pe
		}
		p.checkLang(p.pos, langBashLike|LangMirBSDKorn|LangZsh, "slicing")
		pe.Slice = &Slice{}
		colonPos := p.pos
		p.quote = paramExpArithm
		if p.next(); p.tok != colon {
			// `${x:}` is not a parse error in bash: it reads it and
			// reports "bad substitution" while expanding, which ends
			// the command rather than the script (#277). A slice with
			// neither half is that shape, and the interpreter answers
			// for it.
			if p.tok != rightBrace {
				pe.Slice.Offset = p.followArithm(colon, colonPos)
			}
		}
		colonPos = p.pos
		if p.got(colon) {
			// An empty length is zero — `${x:1:}` and `${x::}` are
			// valid and answer the empty string — so it is a real zero
			// in the tree rather than an absent length, which would
			// mean "to the end".
			if p.tok == rightBrace {
				pe.Slice.Length = &Word{Parts: []WordPart{&Lit{
					ValuePos: p.pos,
					ValueEnd: p.pos,
					Value:    "0",
				}}}
			} else {
				pe.Slice.Length = p.followArithm(colon, colonPos)
			}
		}
		// Need to use a different matched style so arithm errors
		// get reported correctly
		p.quote = old
		pe.Rbrace = p.pos
		p.matchedArithm(pe.Dollar, dollBrace, rightBrace)
		return pe
	case caret, dblCaret, comma, dblComma, parTilde, parDblTilde: // case conversion
		p.checkLang(p.pos, langBashLike, "this expansion operator")
		pe.Exp = p.paramExpExp(old)
	case at, star:
		switch {
		case p.tok == star && !pe.Excl:
			if bad := p.badParamExp(pe, old, p.pos, p.tok.String()); bad != nil {
				return bad
			}
			p.curErr("not a valid parameter expansion operator: %#q", p.tok)
		case pe.Excl && p.r == '}':
			p.checkLang(pe.Pos(), langBashLike, "`${!foo%s}`", p.tok)
			pe.Names = ParNamesOperator(p.tok)
			p.next()
		case p.tok == at:
			p.checkLang(p.pos, langBashLike|LangMirBSDKorn, "this expansion operator")
			fallthrough
		default:
			pe.Exp = p.paramExpExp(old)
		}
	case plus, colPlus, minus, colMinus, quest, colQuest, assgn, colAssgn,
		perc, dblPerc, hash, dblHash, colHash, colPipe, colStar:
		pe.Exp = p.paramExpExp(old)
	case _EOF:
	default:
		// An illegalTok consumed nothing, so the scan starts on the rune
		// the token was read from; anything else was consumed and its
		// source is the token itself.
		consumed := ""
		if p.tok != illegalTok {
			consumed = p.tok.String()
		}
		if bad := p.badParamExp(pe, old, p.pos, consumed); bad != nil {
			return bad
		}
		if paramNameRune(tokRune) {
			if pe.Param != nil {
				p.curErr("%#q cannot be followed by a word", pe.Param.Value)
			} else {
				p.curErr("nested parameter expansion cannot be followed by a word")
			}
		} else {
			p.curErr("not a valid parameter expansion operator: %#q", string(tokRune))
		}
	}
	if p.tok != _EOF && p.tok != rightBrace {
		p.tok = p.paramToken(p.r)
	}
	p.quote = old
	pe.Rbrace = p.matched(pe.Dollar, dollBrace, rightBrace)
	return pe
}

func (p *Parser) paramNameStart() bool {
	r := p.peek()
	if r == utf8.RuneSelf || singleRuneParam(r) || paramNameRune(r) || r == '"' {
		p.rune()
		return true
	}
	return false
}

func (p *Parser) nestedParameterStart(pe *ParamExp) (left token, quotePos Pos) {
	if pe.Short {
		return illegalTok, Pos{}
	}
	if p.r == '"' {
		quotePos = p.nextPos()
		p.rune()
	}
	if p.r != '$' {
		if quotePos.IsValid() {
			return dollar, quotePos
		}
		return illegalTok, Pos{}
	}
	switch p1 := p.peek(); p1 {
	case '{', '(':
		p.pos = p.nextPos()
		if !p.lang.in(LangZsh) {
			// bash reads a nested expansion and complains about it
			// while expanding, so this is parsed the way zsh parses it
			// and the *outer* expansion carries the verdict (#277).
			// Refusing it here forfeited the rest of the file, which is
			// what new-exp.tests measured.
			pe.Bad = true
		}
		p.rune()
		p.rune()
		if p1 == '{' {
			left = dollBrace
		} else { // '('
			left = dollParen
		}
	}
	return left, quotePos
}

func (p *Parser) paramExpParameter(pe *ParamExp) *ParamExp {
	// Check for Zsh nested parameter expressions like ${(f)"$(foo)"}.
	if left, quotePos := p.nestedParameterStart(pe); left != illegalTok {
		var wp WordPart
		switch p.tok = left; p.tok {
		case dollBrace: // ${#${nested parameter}}
			p.tok = dollBrace
			wp = p.paramExp()
		case dollParen: // ${#$(nested command)}
			wp = p.cmdSubst()
		default: // dollar
			p.posErr(pe.Pos(), "invalid nested parameter expansion")
		}
		if quotePos.IsValid() {
			if p.r != '"' {
				p.tok = p.paramToken(p.r)
				if p.tok == illegalTok {
					p.posErr(pe.Pos(), "invalid nested parameter expansion")
				} else {
					p.quoteErr(quotePos, dblQuote)
				}
			}
			pe.NestedParam = &DblQuoted{
				Left:  quotePos,
				Right: p.nextPos(),
				Parts: []WordPart{wp},
			}
			p.rune()
		} else {
			pe.NestedParam = wp
		}
		return pe
	}
	// The parameter name itself, like $foo or $?.
	switch p.r {
	case '?', '-':
		if pe.Length && p.peek() != '}' {
			// actually ${#-default}, not ${#-}; fix the ambiguity
			pe.Length = false
			pos := p.nextPos()
			pe.Param = p.lit(posAddCol(pos, -1), "#")
			pe.Param.ValueEnd = pos
			break
		}
		fallthrough
	case '@', '*', '#', '!', '$':
		r, pos := p.r, p.nextPos()
		p.rune()
		pe.Param = p.lit(pos, string(r))
	default:
		// Note that $1a is equivalent to ${1}a, but ${1a} is not.
		// POSIX Shell says the latter is unspecified behavior, so match Bash's behavior.
		pos := p.nextPos()
		if pe.Short && singleRuneParam(p.r) {
			p.val = string(p.r)
			p.rune()
		} else {
			for p.newLit(p.r); p.r != runeEOF; p.rune() {
				if !paramNameRune(p.r) && p.r != escNewl {
					break
				}
			}
			p.val = p.endLit()
			if !numberLiteral(p.val) && !ValidName(p.val) {
				if pe.Short {
					return nil // just "$"
				}
				if p.lang.in(LangZsh) && p.val == "" {
					// Zsh allows omitting the parameter name, e.g. ${:-word}.
					return pe
				}
				if p.lang.in(langBashLike) {
					// A name bash cannot read is the same runtime verdict
					// as an operator it cannot read: `${1xyz}` and
					// `${#1xyz}` are "bad substitution" while expanding,
					// which loses the command and not the file (#602).
					// What was read stays as the name so the diagnostic
					// can print it back; the rest of the text, if any, is
					// the suffix the operator switch collects.
					pe.Bad = true
					if p.val == "" {
						return pe
					}
				} else {
					p.posErr(pos, "invalid parameter name")
				}
			}
		}
		pe.Param = p.lit(pos, p.val)
	}
	return pe
}

// dquoteLike reports whether state is one where an expansion is being
// read inside double quotes. A here-document body counts: bash treats it
// as a double-quoted context, and answers `${x+\'}` there the same way
// (measured in both modes).
func dquoteLike(state quoteState) bool {
	switch state {
	case dblQuotes, hdocBody, hdocBodyTabs:
		return true
	}
	return false
}

// patternParExpOp reports whether op takes a pattern rather than a plain
// word, which is where bash keeps single quotes special even in posix
// mode -- quoting a pattern character is how it is made literal, so the
// quotes are doing work that `${x-\'}`'s are not.
func patternParExpOp(op ParExpOperator) bool {
	switch op {
	case RemSmallPrefix, RemLargePrefix, RemSmallSuffix, RemLargeSuffix,
		UpperFirst, UpperAll, LowerFirst, LowerAll:
		return true
	}
	return false
}

// paramExpExp parses the word of a `${name<op>word}` expansion. outer is
// the lexer state the expansion itself was found in, which is what says
// whether it is inside double quotes.
func (p *Parser) paramExpExp(outer quoteState) *Expansion {
	op := ParExpOperator(p.tok)
	switch op {
	case MatchEmpty, ArrayExclude, ArrayIntersect:
		p.checkLang(p.pos, LangZsh, "${name%sarg}", op)
	}
	p.quote = paramExpExp
	// In posix mode a single quote here is an ordinary character rather
	// than the start of a quoted span, so `"${IFS+'bar} baz"` is a whole
	// word instead of a scan to EOF looking for a closing quote (#450).
	// It applies to the substitution operators only: bash keeps quotes
	// special for the pattern ones, where the quoting decides what the
	// pattern matches, and both halves were measured.
	oldSgl := p.sglQuoteLiteral
	p.sglQuoteLiteral = p.posix && dquoteLike(outer) && !patternParExpOp(op)
	defer func() { p.sglQuoteLiteral = oldSgl }()
	p.next()
	// bash never looks at the `@` transform's letter while parsing: it
	// takes the text between the `@` and the brace and decides when it
	// expands, and on a parameter with no value it does not decide at all
	// — `${x@nope}` on an unset x is the empty string at status 0, so
	// refusing it here was strictly wrong rather than merely early
	// (#602). The other languages keep the parse-time check: mksh has
	// only `${x@#}`, and posix has no `@` operator to reach this with.
	if op == OtherParamOps && !p.lang.in(langBashLike) {
		if !p.tok.isLit() {
			p.curErr("@ expansion operator requires a literal")
		}
		switch p.val {
		case "a", "k", "u", "A", "E", "K", "L", "P", "U":
			p.checkLang(p.pos, langBashLike, "this expansion operator")
		case "#":
			p.checkLang(p.pos, LangMirBSDKorn, "this expansion operator")
		case "Q":
		default:
			p.curErr("invalid @ expansion operator %#q", p.val)
		}
	}
	return &Expansion{Op: op, Word: p.getWord()}
}

// eitherIndex parses a subscript — `[i]` or `["k"]` — and reports
// whether the brackets held *nothing at all*.
//
// That second answer exists because bash distinguishes `[]` from `[ ]`
// (#582), which looks arbitrary until you see where it comes from: the
// subscript is a word evaluated when the assignment or expansion runs,
// and bash checks it for emptiness before evaluating. So `b[]=x` is a
// runtime `b[]: bad array subscript` while `b[  ]=x` writes index 0,
// since an empty arithmetic expression is zero — the same rule that
// makes `$(( ))` answer 0. A nil expression carries the blank case,
// which every arithmetic reader already treats as zero; the caller
// decides what to do about the empty one, and only an assignment target
// can defer it.
func (p *Parser) eitherIndex(deferEmpty bool) (ArithmExpr, bool) {
	old := p.quote
	lpos := p.pos
	if p.lang.in(LangZsh) && p.r == '(' {
		return p.zshFlagsIndex(lpos, old)
	}
	startPos, raw, closed := p.subscriptText()
	empty := raw == ""
	if !closed {
		// The brackets never closed. An empty subscript is the error the
		// arithmetic parser used to give — nothing followed the `[` —
		// and anything else is an unmatched bracket, named after the
		// token the scan ran into.
		p.quote = paramExpArithm
		p.next()
		p.quote = old
		if empty {
			p.followErrExp(lpos, leftBrack)
		} else {
			p.arithmMatchingErr(lpos, leftBrack, rightBrack)
		}
		return nil, false
	}
	if empty && !deferEmpty {
		p.quote = old
		p.posErr(lpos, "%#q must be followed by an expression", leftBrack)
		return nil, false
	}
	var expr ArithmExpr
	if !empty {
		expr = p.subscriptExpr(startPos, raw)
	}
	// The scan stopped *on* the closing bracket rather than consuming
	// it, so the token the caller matches against still has to be read —
	// as a token, which only the state a subscript is read in makes it:
	// outside one a `]` is an ordinary character, and the outer state is
	// runeByRune for a `${name[i]}`, where nothing is a token at all.
	p.quote = paramExpArithm
	p.next()
	p.quote = old
	p.matchedArithm(lpos, leftBrack, rightBrack)
	return expr, empty
}

// zshFlagsIndex reads a zsh subscript that opens with flags —
// `${signals[(i)QUIT]}` — through the arithmetic parser, the way every
// subscript used to be read.
//
// Flags are the one shape the raw scan cannot find the end of: the `)`
// closing the flag list would read as the end of the construct the
// subscript sits in, and zsh's flag argument is a *pattern* rather than
// a word, so [Parser.zshSubFlags] lexes it as raw text of its own. The
// shape is recognizable before the scan starts — a `(` immediately after
// the `[` — which is exactly when the arithmetic parser reached
// zshSubFlags before (#564).
func (p *Parser) zshFlagsIndex(lpos Pos, old quoteState) (ArithmExpr, bool) {
	p.quote = paramExpArithm
	p.next()
	expr := p.followArithm(leftBrack, lpos)
	p.quote = old
	p.matchedArithm(lpos, leftBrack, rightBrack)
	return expr, false
}

// subscriptText scans a subscript's raw source to its matching `]`,
// which is how bash reads one: whether the text is an arithmetic
// expression or an associative key is not knowable until the array it
// belongs to is known, so nothing can be decided while reading it
// (#564). The scan honours quotes, escapes and nested brackets, since
// `m[x[1]]` names the key `x[1]` and `m[$(echo "a]b")]` names `a]b`.
//
// It reports where the text starts, for the sub-parse, and whether the
// bracket was found at all — a scan that runs into the end of the input,
// or out of the construct the subscript sits in, stops there and leaves
// the caller to report it.
//
// Only `]`, a quote and a backslash are significant to the scan, because
// only those are significant to bash's: every metacharacter is ordinary
// text in a subscript, measured, even the ones that end the construct the
// subscript is written in. `m[a}b]` inside `${m[a}b]}` is the key `a}b`,
// and `m[a)b]` is the key `a)b` inside `$( … )` and inside `m=( … )`
// alike. Arithmetic is the exception, and not because of the subscript:
// bash finds the end of a `$(( … ))` by matching parentheses before
// anything inside it is read, so `$((m[a)b]))` is a syntax error there
// while the same subscript is a key everywhere else. That is why a `)`
// only stops the scan when the subscript sits in arithmetic.
func (p *Parser) subscriptText() (Pos, string, bool) {
	oldQuote := p.quote
	p.quote = runeByRune
	defer func() { p.quote = oldQuote }()
	p.pos = p.nextPos()
	startPos := p.pos
	p.newLit(p.r)
	depth := 0
	// closers is what a nested construct inside the subscript is waiting
	// for, so that the `)` of a `$(…)` or of a parenthesized arithmetic
	// subexpression is not read as the end of the arithmetic the
	// subscript itself is in.
	var closers []rune
	for {
		switch p.r {
		case runeEOF:
			return startPos, p.endLit(), false
		case '(':
			closers = append(closers, ')')
		case ')':
			if len(closers) == 0 {
				if oldQuote&allArithmExpr != 0 {
					return startPos, p.endLit(), false
				}
				break
			}
			if closers[len(closers)-1] == ')' {
				closers = closers[:len(closers)-1]
			}
		case '`':
			if len(closers) > 0 && closers[len(closers)-1] == '`' {
				closers = closers[:len(closers)-1]
			} else {
				closers = append(closers, '`')
			}
		case '\\':
			p.rune() // whatever follows is literal, including `]`
		case '\'':
			for p.rune(); p.r != runeEOF && p.r != '\''; p.rune() {
			}
			if p.r == runeEOF {
				return startPos, p.endLit(), false
			}
		case '"':
			for p.rune(); p.r != runeEOF && p.r != '"'; p.rune() {
				if p.r == '\\' {
					p.rune()
				}
			}
			if p.r == runeEOF {
				return startPos, p.endLit(), false
			}
		case '[':
			depth++
		case ']':
			if depth == 0 {
				return startPos, p.endLit(), true
			}
			depth--
		}
		p.rune()
	}
}

// braceText scans the raw source of a `${…}` suffix to the brace that
// closes the expansion, which is how bash reads one: it takes the text
// first and decides what the text means afterwards, so a suffix it is
// going to refuse still has to be read to the end (#602). It reports the
// text, which excludes the closing brace, and whether that brace was
// found at all.
//
// Only a brace, a quote and a backslash are significant, because only
// those are significant to bash's own scan, measured: `${H*"}"}`,
// `${H*'}'}` and `${H*\}}` all end at the last brace, `${H*{a}}` counts
// the one in between, and `${H*{a}` — where the count never comes back
// down — is the same unmatched-brace error `${H*` is. The sibling scan
// one layer in is [Parser.subscriptText], for the same reason.
func (p *Parser) braceText() (string, bool) {
	old := p.quote
	p.quote = runeByRune
	defer func() { p.quote = old }()
	p.newLit(p.r)
	depth := 0
	for {
		switch p.r {
		case runeEOF:
			return p.endLit(), false
		case '\\':
			p.rune() // whatever follows is literal, including `}`
		case '\'':
			for p.rune(); p.r != runeEOF && p.r != '\''; p.rune() {
			}
			if p.r == runeEOF {
				return p.endLit(), false
			}
		case '"':
			for p.rune(); p.r != runeEOF && p.r != '"'; p.rune() {
				if p.r == '\\' {
					p.rune()
				}
			}
			if p.r == runeEOF {
				return p.endLit(), false
			}
		case '{':
			depth++
		case '}':
			if depth == 0 {
				return p.endLit(), true
			}
			depth--
		}
		p.rune()
	}
}

// badParamExp finishes a `${…}` whose suffix is text bash reads and only
// refuses once it expands it, naming the whole expansion as written
// (#602). It returns nil when this language has no such verdict to defer
// to, leaving the caller's parse error in place.
//
// consumed is the source of the operator token the caller has already
// lexed, if any, and pos is where that token — or the rune the scan is
// about to start on — begins. Both go into [ParamExp.BadSuffix], which
// is what prints the expansion back.
//
// Refusing these while parsing is what cost cond.tests and
// more-exp.tests the rest of the file rather than the line, since koi
// parses ahead of what it runs; the marker is #277's and the interpreter
// already routes it to #469's input-unit abandonment.
func (p *Parser) badParamExp(pe *ParamExp, outer quoteState, pos Pos, consumed string) *ParamExp {
	if !p.lang.in(langBashLike) {
		return nil
	}
	raw, closed := p.braceText()
	if !closed {
		// The brace never closed, which is an error in bash too — it is
		// the input that ran out rather than a shape bash has a verdict
		// for, so the caller's report stands.
		return nil
	}
	pe.Bad = true
	if raw = consumed + raw; raw != "" {
		pe.BadSuffix = p.lit(pos, raw)
	}
	p.pos = p.nextPos()
	p.tok = p.paramToken(p.r) // the `}` the scan stopped at
	pe.Rbrace = p.pos
	p.quote = outer
	p.next()
	return pe
}

// subscriptExpr is the subscript's text as a node: an arithmetic
// expression when it reads as one, and otherwise the word it spells,
// which is what lets `m[hello world]` and `m[%]` be keys instead of
// parse errors (#564). An indexed array reaching a word that is not
// arithmetic is bash's runtime `arithmetic syntax error in expression`,
// raised where the assignment runs.
//
// Both halves are parsed by a sub-parser seeded with the text's real
// position, so a diagnostic about a subscript names the column it is at.
func (p *Parser) subscriptExpr(startPos Pos, raw string) ArithmExpr {
	// [Parser.Arithmetic] takes the whole text or nothing, so a subscript
	// that only *starts* as arithmetic — `hello world` reads `hello` and
	// stops — falls through to the word rather than silently losing its
	// second half.
	if expr, err := p.fragment(startPos).Arithmetic(strings.NewReader(raw)); err == nil {
		return expr
	}
	sub := p.fragment(startPos)
	w, err := sub.wholeWord(strings.NewReader(raw))
	if err != nil {
		// The text cannot be read either way — an unterminated quote or
		// expansion inside it — and that is a parse error like any
		// other, reported where it is.
		p.err = err
		return nil
	}
	return w
}

// fragment is a parser for a piece of the input this one has already
// lexed, positioned as if the piece had been parsed in place.
func (p *Parser) fragment(startPos Pos) *Parser {
	sub := NewParser(Variant(p.lang))
	if p.posix {
		POSIXMode(true)(sub)
	}
	sub.fragmentPos = startPos
	return sub
}

// wholeWord parses the input as a single word: spaces are ordinary
// characters and quotes are not, which is a subscript's text and a
// `${name+word}`'s word alike.
func (p *Parser) wholeWord(r io.Reader) (*Word, error) {
	p.reset()
	p.f = &File{}
	p.src = r
	p.rune()
	p.quote = subscriptWord
	p.next()
	w := p.getWord()
	if p.err == nil && p.tok != _EOF {
		p.curErr("%#q is not a valid subscript", p.tok)
	}
	return w, p.err
}

func (p *Parser) zshSubFlags() *FlagsArithm {
	zf := &FlagsArithm{}
	// Lex flags as raw text, like paramExp does for ${(flags)...}.
	lparen := p.pos
	old := p.quote
	p.quote = runeByRune
	p.pos = p.nextPos()
	for p.newLit(p.r); p.r != runeEOF && p.r != ')'; p.rune() {
	}
	p.val = p.endLit()
	if p.r != ')' {
		p.tok = _EOF
		p.matchingErr(lparen, leftParen, rightParen)
	}
	zf.Flags = p.lit(p.pos, p.val)
	p.rune()
	// Lex the argument as a raw pattern, stopping at ',' or ']',
	// since zsh treats it as a pattern rather than an arithmetic expression.
	argPos := p.nextPos()
	for p.newLit(p.r); p.r != runeEOF && p.r != ',' && p.r != ']'; p.rune() {
	}
	if val := p.endLit(); val != "" {
		zf.X = p.wordOne(p.lit(argPos, val))
	}
	p.quote = old
	p.next()
	return zf
}

func (p *Parser) stopToken() bool {
	switch p.tok {
	case _EOF, _Newl, semicolon, and, or, andAnd, orOr, orAnd, andPipe, andBang,
		dblSemicolon, semiAnd, dblSemiAnd, semiOr, rightParen:
		return true
	case bckQuote:
		return p.backquoteEnd()
	}
	return false
}

func (p *Parser) backquoteEnd() bool {
	return p.lastBquoteEsc < p.openBquotes
}

// ValidName returns whether val is a valid name as per the POSIX spec.
func ValidName(val string) bool {
	if val == "" {
		return false
	}
	for i, r := range val {
		switch {
		case asciiLetter(r), r == '_':
		case i > 0 && asciiDigit(r):
		default:
			return false
		}
	}
	return true
}

func numberLiteral[T string | []byte](val T) bool {
	if len(val) == 0 {
		return false
	}
	for _, r := range string(val) {
		if !asciiDigit(r) {
			return false
		}
	}
	return true
}

func (p *Parser) hasValidIdent() bool {
	if !p.tok.isLit() {
		return false
	}
	if end := p.eqlOffs; end > 0 {
		if p.val[end-1] == '+' && p.lang.in(langBashLike|LangMirBSDKorn|LangZsh) {
			end-- // a+=x
		}
		if ValidName(p.val[:end]) {
			return true
		}
	} else if !ValidName(p.val) {
		return false // *[i]=x
	}
	return p.r == '[' // a[i]=x
}

func (p *Parser) getAssign(needEqual bool) *Assign {
	as := &Assign{}
	if p.eqlOffs > 0 { // foo=bar
		nameEnd := p.eqlOffs
		if p.lang.in(langBashLike|LangMirBSDKorn|LangZsh) && p.val[p.eqlOffs-1] == '+' {
			// a+=b
			as.Append = true
			nameEnd--
		}
		as.Name = p.lit(p.pos, p.val[:nameEnd])
		// since we're not using the entire p.val
		as.Name.ValueEnd = posAddCol(as.Name.ValuePos, nameEnd)
		left := p.lit(posAddCol(p.pos, 1), p.val[p.eqlOffs+1:])
		if left.Value != "" {
			left.ValuePos = posAddCol(left.ValuePos, p.eqlOffs)
			as.Value = p.wordOne(left)
		}
		p.next()
	} else { // foo[x]=bar
		as.Name = p.lit(p.pos, p.val)
		// hasValidIdent already checks p.r is '['
		p.rune()
		p.pos = posAddCol(p.pos, 1)
		as.Index, as.BadIndex = p.eitherIndex(true)
		if p.spaced || p.stopToken() {
			if as.BadIndex {
				// `a[]` with no assignment is not a target at all, so
				// there is nothing to defer the empty subscript to and
				// it keeps being a parse error (#582).
				p.posErr(as.Name.End(), "%#q must be followed by an expression", leftBrack)
				return nil
			}
			if needEqual {
				p.followErr(as.Pos(), "a[b]", assgn)
			} else {
				as.Naked = true
				return as
			}
		}
		if p.tok == assgnParen {
			// `a[i]=(values)` is zsh's, and in bash it is an error the
			// *assignment* reports — `a[i]: cannot assign list to array
			// member` — so the shape is parsed in both and the verdict
			// belongs to the interpreter (#582). Refusing it here cost
			// the rest of the file for a construct bash reads.
			//
			// assgnParen consumed both '=' and '(', so rewrite as
			// leftParen for the array parsing below.
			p.tok = leftParen
			p.pos = posAddCol(p.pos, 1)
		} else {
			if len(p.val) > 0 && p.val[0] == '+' {
				as.Append = true
				p.val = p.val[1:]
				p.pos = posAddCol(p.pos, 1)
			}
			if len(p.val) < 1 || p.val[0] != '=' {
				if as.Append {
					p.followErr(as.Pos(), "a[b]+", assgn)
				} else {
					p.followErr(as.Pos(), "a[b]", assgn)
				}
				return nil
			}
			p.pos = posAddCol(p.pos, 1)
			p.val = p.val[1:]
			if p.val == "" {
				p.next()
			}
		}
	}
	if p.spaced || p.stopToken() {
		return as
	}
	if as.Value == nil && p.tok == leftParen {
		p.checkLang(p.pos, langBashLike|LangMirBSDKorn|LangZsh, "arrays")
		as.Array = &ArrayExpr{Lparen: p.pos}
		newQuote := p.quote
		if p.lang.in(langBashLike | LangZsh) {
			newQuote = arrayElems
		}
		old := p.preNested(newQuote)
		p.next()
		p.got(_Newl)
		badElems := false
		for p.tok != _EOF && p.tok != rightParen {
			ae := &ArrayElem{}
			ae.Comments, p.accComs = p.accComs, nil
			if p.tok == leftBrack {
				left := p.pos
				// `[]=v` inside a compound assignment is bash's
				// runtime error rather than a parse error (#582), the
				// same as a bare `name[]=v`.
				ae.Index, ae.BadIndex = p.eitherIndex(true)
				if p.tok == assgnParen {
					p.curErr("arrays cannot be nested")
					return nil
				}
				p.follow(left, `[x]`, assgn)
			}
			if ae.Value = p.getWord(); ae.Value == nil {
				switch p.tok {
				case _Newl, rightParen, leftBrack:
					// TODO: support [index]=[
				default:
					// bash reports this and keeps reading (#581):
					// `x=(a & b)` costs the line and the script carries
					// on, so the error is recoverable and the parse
					// skips to the array's own closing paren rather than
					// forfeiting the rest of the file.
					p.recoverableErr(p.pos, "syntax error near unexpected token `%s'", p.tok)
					// A nested `(` is itself an unexpected token in bash
					// — it is what the error above just named — so the
					// depth count is only here to find the paren that
					// closes *this* array rather than an inner one.
					for depth := 0; p.tok != _EOF; p.next() {
						if p.tok == leftParen {
							depth++
						} else if p.tok == rightParen {
							if depth == 0 {
								break
							}
							depth--
						}
					}
					badElems = true
				}
			}
			if badElems {
				break
			}
			if len(p.accComs) > 0 {
				c := p.accComs[0]
				if c.Pos().Line() == ae.End().Line() {
					ae.Comments = append(ae.Comments, c)
					p.accComs = p.accComs[1:]
				}
			}
			as.Array.Elems = append(as.Array.Elems, ae)
			p.got(_Newl)
		}
		as.Array.Last, p.accComs = p.accComs, nil
		p.postNested(old)
		as.Array.Rparen = p.matched(as.Array.Lparen, leftParen, rightParen)
	} else if w := p.getWord(); w != nil {
		if as.Value == nil {
			as.Value = w
		} else {
			as.Value.Parts = append(as.Value.Parts, w.Parts...)
		}
	}
	return as
}

func (p *Parser) peekRedir() bool {
	switch p.tok {
	case _LitRedir, rdrOut, appOut, rdrIn, rdrInOut, dplIn, dplOut,
		rdrClob, appClob, hdoc, dashHdoc, wordHdoc,
		rdrAll, rdrAllClob, appAll, appAllClob:
		return true
	}
	return false
}

func (p *Parser) doRedirect(s *Stmt) {
	var r *Redirect
	if s.Redirs == nil {
		var alloc struct {
			redirs [4]*Redirect
			redir  Redirect
		}
		s.Redirs = alloc.redirs[:0]
		r = &alloc.redir
		s.Redirs = append(s.Redirs, r)
	} else {
		r = &Redirect{}
		s.Redirs = append(s.Redirs, r)
	}
	r.N = p.getLit()
	if r.N != nil && r.N.Value[0] == '{' {
		p.checkLang(r.N.Pos(), langBashLike|LangZsh, "`{varname}` redirects")
	}
	r.Op, r.OpPos = RedirOperator(p.tok), p.pos
	switch r.Op {
	case RdrAll, AppAll:
		p.checkLang(p.pos, langBashLike|LangMirBSDKorn|LangZsh, "%#q redirects", r.Op)
	case AppClob, RdrAllClob, AppAllClob:
		p.checkLang(p.pos, LangZsh, "%#q redirects", r.Op)
	}
	p.next()
	switch r.Op {
	case Hdoc, DashHdoc:
		old := p.quote
		p.quote, p.forbidNested = hdocWord, true
		p.heredocs = append(p.heredocs, r)
		r.Word = p.followWordTok(token(r.Op), r.OpPos)
		p.quote, p.forbidNested = old, false
		if p.tok == _Newl {
			if len(p.accComs) > 0 {
				c := p.accComs[0]
				if c.Pos().Line() == s.End().Line() {
					s.Comments = append(s.Comments, c)
					p.accComs = p.accComs[1:]
				}
			}
			p.doHeredocs()
		}
	case WordHdoc:
		p.checkLang(r.OpPos, langBashLike|LangMirBSDKorn|LangZsh, "herestrings")
		fallthrough
	default:
		r.Word = p.followWordTok(token(r.Op), r.OpPos)
	}
}

func (p *Parser) getStmt(readEnd, binCmd, fnBody bool) *Stmt {
	pos, ok := p.gotRsrv("!")
	s := &Stmt{Position: pos}
	if ok {
		s.Negated = true
		if p.stopToken() {
			p.posErr(s.Pos(), `%#q cannot form a statement alone`, exclMark)
		}
		if _, ok := p.gotRsrv("!"); ok {
			p.posErr(s.Pos(), `cannot negate a command multiple times`)
		}
	}
	if s = p.gotStmtPipe(s, false); s == nil || p.err != nil {
		return nil
	}
	// instead of using recursion, iterate manually
	for p.tok == andAnd || p.tok == orOr {
		if binCmd {
			// left associativity: in a list of BinaryCmds, the
			// right recursion should only read a single element
			return s
		}
		b := &BinaryCmd{
			OpPos: p.pos,
			Op:    BinCmdOperator(p.tok),
			X:     s,
		}
		p.next()
		p.got(_Newl)
		b.Y = p.getStmt(false, true, false)
		if b.Y == nil || p.err != nil {
			if p.recoverError() {
				b.Y = &Stmt{Position: recoveredPos}
			} else {
				p.followErr(b.OpPos, b.Op, noQuote("a statement"))
				return nil
			}
		}
		s = &Stmt{Position: s.Position}
		s.Cmd = b
		s.Comments, b.X.Comments = b.X.Comments, nil
	}
	if readEnd {
		switch p.tok {
		case semicolon:
			s.Semicolon = p.pos
			p.next()
		case and:
			s.Semicolon = p.pos
			p.next()
			s.Background = true
		case orAnd:
			s.Semicolon = p.pos
			p.next()
			s.Coprocess = true
		case andPipe, andBang:
			s.Semicolon = p.pos
			p.next()
			s.Disown = true
		}
	}
	if len(p.accComs) > 0 && !binCmd && !fnBody {
		c := p.accComs[0]
		if c.Pos().Line() == s.End().Line() {
			s.Comments = append(s.Comments, c)
			p.accComs = p.accComs[1:]
		}
	}
	return s
}

func (p *Parser) gotStmtPipe(s *Stmt, binCmd bool) *Stmt {
	s.Comments, p.accComs = p.accComs, nil
	for p.peekRedir() {
		p.doRedirect(s)
	}
	redirsStart := len(s.Redirs)
	switch p.tok {
	case _LitWord:
		switch p.val {
		case "{":
			p.block(s)
		case "{}":
			// Zsh treats closing braces in a special way, allowing this.
			if p.lang.in(LangZsh) {
				s.Cmd = &Block{Lbrace: p.pos, Rbrace: posAddCol(p.pos, 1)}
				p.next()
			}
		case "if":
			p.ifClause(s)
		case "while", "until":
			// TODO(zsh): "repeat"
			p.whileClause(s, p.val == "until")
		case "for":
			p.forClause(s)
		case "case":
			p.caseClause(s)
		// TODO(zsh): { try-list } "always" { always-list }
		case "}":
			p.curErr(`%#q can only be used to close a block`, rightBrace)
		case "then", "elif":
			p.curErr("%#q can only be used in an `if`", p.val)
		case "fi":
			p.curErr("%#q can only be used to end an `if`", p.val)
		case "do":
			p.curErr(`%#q can only be used in a loop`, p.val)
		case "done":
			p.curErr(`%#q can only be used to end a loop`, p.val)
		case "esac":
			p.curErr("%#q can only be used to end a `case`", p.val)
		case "!":
			if !s.Negated {
				p.curErr(`%#q can only be used in full statements`, exclMark)
				break
			}
		case "[[":
			if p.lang.in(langBashLike | LangMirBSDKorn | LangZsh) {
				p.testClause(s)
			}
		case "]]":
			if p.lang.in(langBashLike | LangMirBSDKorn | LangZsh) {
				p.curErr(`%#q can only be used to close a test`, dblRightBrack)
			}
		case "let":
			if p.lang.in(langBashLike | LangMirBSDKorn | LangZsh) {
				p.letClause(s)
			}
		case "function":
			if p.lang.in(langBashLike | LangMirBSDKorn | LangZsh) {
				p.bashFuncDecl(s)
			}
		case "declare":
			if p.lang.in(langBashLike | LangZsh) { // Note that mksh lacks this one.
				p.declClause(s)
			}
		case "local", "export", "readonly", "typeset", "nameref":
			if p.lang.in(langBashLike | LangMirBSDKorn | LangZsh) {
				p.declClause(s)
			}
		case "time":
			if p.lang.in(langBashLike | LangMirBSDKorn | LangZsh) {
				p.timeClause(s)
			}
		case "coproc":
			if p.lang.in(langBashLike) { // Note that mksh lacks this one.
				p.coprocClause(s)
			}
		case "select":
			if p.lang.in(langBashLike | LangMirBSDKorn | LangZsh) {
				p.selectClause(s)
			}
		case "@test":
			if p.lang.in(LangBats) {
				p.testDecl(s)
			}
		}
		if s.Cmd != nil {
			break
		}
		if p.hasValidIdent() {
			p.callExpr(s, nil, true)
			break
		}
		name := p.lit(p.pos, p.val)
		p.next()
		// In zsh, ( after a word is a glob qualifier unless followed
		// immediately by ), which is the func declaration syntax.
		if p.tok == leftParen && (!p.lang.in(LangZsh) || p.r == ')') {
			p.next()
			p.follow(name.ValuePos, "foo(", rightParen)
			if p.lang.in(LangPOSIX) && !ValidName(name.Value) {
				p.posErr(name.Pos(), "invalid func name")
			}
			p.funcDecl(s, name.ValuePos, false, true, name)
		} else {
			w := p.wordOne(name)
			if p.lang.in(LangZsh) && !p.spaced {
				w.Parts = append(w.Parts, p.wordParts(nil)...)
			}
			p.callExpr(s, w, false)
		}
	case bckQuote:
		if p.backquoteEnd() {
			break
		}
		fallthrough
	case _Lit, dollBrace, dollDblParen, dollParen, dollar, cmdIn, assgnParen, cmdOut,
		sglQuote, dollSglQuote, dblQuote, dollDblQuote, dollBrack,
		globQuest, globStar, globPlus, globAt, globExcl:
		if p.hasValidIdent() {
			p.callExpr(s, nil, true)
			break
		}
		w := p.wordAnyNumber()
		if p.got(leftParen) {
			p.posErr(w.Pos(), "invalid func name")
		}
		p.callExpr(s, w, false)
	case leftParen:
		if p.r == ')' {
			p.rune()
			fpos := p.pos
			p.next()
			if p.tok == _LitWord && p.val == "{" {
				p.checkLang(fpos, LangZsh, "anonymous functions")
			}
			p.funcDecl(s, fpos, false, true)
			break
		}
		p.subshell(s)
	case dblLeftParen:
		p.arithmExpCmd(s)
	}
	if s.Cmd == nil && len(s.Redirs) == 0 {
		return nil // no statement found
	}
	if redirsStart > 0 && s.Cmd != nil {
		if _, ok := s.Cmd.(*CallExpr); !ok {
			p.checkLang(s.Pos(), LangZsh, "redirects before compound commands")
		}
	}
	for p.peekRedir() {
		p.doRedirect(s)
	}
	// instead of using recursion, iterate manually
	for p.tok == or || p.tok == orAnd {
		if binCmd {
			// left associativity: in a list of BinaryCmds, the
			// right recursion should only read a single element
			return s
		}
		if p.tok == orAnd && p.lang.in(LangMirBSDKorn) {
			// No need to check for LangPOSIX, as on that language
			// we parse |& as two tokens.
			break
		}
		b := &BinaryCmd{OpPos: p.pos, Op: BinCmdOperator(p.tok), X: s}
		p.next()
		p.got(_Newl)
		if b.Y = p.gotStmtPipe(&Stmt{Position: p.pos}, true); b.Y == nil || p.err != nil {
			if p.recoverError() {
				b.Y = &Stmt{Position: recoveredPos}
			} else {
				p.followErr(b.OpPos, b.Op, noQuote("a statement"))
				break
			}
		}
		s = &Stmt{Position: s.Position}
		s.Cmd = b
		s.Comments, b.X.Comments = b.X.Comments, nil
		// in "! x | y", the bang applies to the entire pipeline
		s.Negated = b.X.Negated
		b.X.Negated = false
	}
	return s
}

func (p *Parser) subshell(s *Stmt) {
	sub := &Subshell{Lparen: p.pos}
	old := p.preNested(subCmd)
	p.next()
	sub.Stmts, sub.Last = p.followStmts("(", sub.Lparen)
	p.postNested(old)
	sub.Rparen = p.matched(sub.Lparen, leftParen, rightParen)
	s.Cmd = sub
}

func (p *Parser) arithmExpCmd(s *Stmt) {
	ar := &ArithmCmd{Left: p.pos}
	old := p.preNested(arithmExprCmd)
	p.next()
	if p.got(hash) {
		p.checkLang(ar.Pos(), LangMirBSDKorn, "unsigned expressions")
		ar.Unsigned = true
	}
	// `(( ))` is empty and valid, like `$(())` above; it evaluates
	// as zero, so the command's status is 1.
	if !p.peekArithmEnd() {
		ar.X = p.followArithm(dblLeftParen, ar.Left)
	}
	ar.Right = p.arithmEnd(dblLeftParen, ar.Left, old)
	s.Cmd = ar
}

func (p *Parser) block(s *Stmt) {
	b := &Block{Lbrace: p.pos}
	p.next()
	b.Stmts, b.Last = p.followStmts("{", b.Lbrace, "}")
	if pos, ok := p.gotRsrv("}"); ok {
		b.Rbrace = pos
	} else if p.recoverError() {
		b.Rbrace = recoveredPos
	} else {
		p.matchingErr(b.Lbrace, leftBrace, rightBrace)
	}
	s.Cmd = b
}

func (p *Parser) ifClause(s *Stmt) {
	rootIf := &IfClause{Position: p.pos}
	p.next()
	rootIf.Cond, rootIf.CondLast = p.followStmts("if", rootIf.Position, "then")
	rootIf.ThenPos = p.followRsrv(rootIf.Position, "if <cond>", "then")
	rootIf.Then, rootIf.ThenLast = p.followStmts("then", rootIf.ThenPos, "fi", "elif", "else")
	curIf := rootIf
	for p.tok == _LitWord && p.val == "elif" {
		elf := &IfClause{Position: p.pos}
		curIf.Last = p.accComs
		p.accComs = nil
		p.next()
		elf.Cond, elf.CondLast = p.followStmts("elif", elf.Position, "then")
		elf.ThenPos = p.followRsrv(elf.Position, "elif <cond>", "then")
		elf.Then, elf.ThenLast = p.followStmts("then", elf.ThenPos, "fi", "elif", "else")
		curIf.Else = elf
		curIf = elf
	}
	if elsePos, ok := p.gotRsrv("else"); ok {
		curIf.Last = p.accComs
		p.accComs = nil
		els := &IfClause{Position: elsePos}
		els.Then, els.ThenLast = p.followStmts("else", els.Position, "fi")
		curIf.Else = els
		curIf = els
	}
	curIf.Last = p.accComs
	p.accComs = nil
	rootIf.FiPos = p.stmtEnd(rootIf, "if", "fi")
	for els := rootIf.Else; els != nil; els = els.Else {
		// All the nested IfClauses share the same FiPos.
		els.FiPos = rootIf.FiPos
	}
	s.Cmd = rootIf
}

func (p *Parser) whileClause(s *Stmt, until bool) {
	wc := &WhileClause{WhilePos: p.pos, Until: until}
	rsrv := "while"
	rsrvCond := "while <cond>"
	if wc.Until {
		rsrv = "until"
		rsrvCond = "until <cond>"
	}
	p.next()
	wc.Cond, wc.CondLast = p.followStmts(rsrv, wc.WhilePos, "do")
	wc.DoPos = p.followRsrv(wc.WhilePos, rsrvCond, "do")
	wc.Do, wc.DoLast = p.followStmts("do", wc.DoPos, "done")
	wc.DonePos = p.stmtEnd(wc, rsrv, "done")
	s.Cmd = wc
}

func (p *Parser) forClause(s *Stmt) {
	fc := &ForClause{ForPos: p.pos}
	p.next()
	fc.Loop = p.loop(fc.ForPos)

	start, end := "do", "done"
	if pos, ok := p.gotRsrv("{"); ok {
		p.checkLang(pos, langBashLike|LangMirBSDKorn, "for loops with braces")
		fc.DoPos = pos
		fc.Braces = true
		start, end = "{", "}"
	} else {
		fc.DoPos = p.followRsrv(fc.ForPos, "for foo [in words]", start)
	}

	s.Comments = append(s.Comments, p.accComs...)
	p.accComs = nil
	fc.Do, fc.DoLast = p.followStmts(start, fc.DoPos, end)
	fc.DonePos = p.stmtEnd(fc, "for", end)
	s.Cmd = fc
}

func (p *Parser) loop(fpos Pos) Loop {
	switch p.tok {
	case leftParen, dblLeftParen:
		p.checkLang(p.pos, langBashLike|LangZsh, "c-style fors")
	}
	if p.tok == dblLeftParen {
		cl := &CStyleLoop{Lparen: p.pos}
		old := p.preNested(arithmExprCmd)
		p.next()
		cl.Init = p.arithmExpr(false)
		if !p.got(dblSemicolon) {
			p.follow(p.pos, "expr", semicolon)
			cl.Cond = p.arithmExpr(false)
			p.follow(p.pos, "expr", semicolon)
		}
		cl.Post = p.arithmExpr(false)
		cl.Rparen = p.arithmEnd(dblLeftParen, cl.Lparen, old)
		p.got(semicolon)
		p.got(_Newl)
		return cl
	}
	return p.wordIter("for", fpos)
}

func (p *Parser) wordIter(ftok string, fpos Pos) *WordIter {
	wi := &WordIter{}
	// The whole word, then decide what it is. bash reads *any* word here
	// and refuses it when the loop runs — `for $1 in a` reports
	// `` `$1': not a valid identifier ``, with the text as written since
	// it never expands it, and the shell carries on (#593). Refusing at
	// parse time cost the rest of the file, which is #277's shape; the
	// interpreter already gives that exact message for a literal that is
	// not a name (#409).
	//
	// A word rather than a literal is also what makes `for x$1 in a`
	// one name instead of the literal `x` followed by a stray `$1`.
	switch w := p.getWord(); {
	case w == nil:
		p.followErr(fpos, ftok, noQuote("a literal"))
	case len(w.Parts) == 1:
		if lit, ok := w.Parts[0].(*Lit); ok {
			wi.Name = lit
		} else {
			wi.BadName = w
		}
	default:
		wi.BadName = w
	}
	if p.got(semicolon) {
		p.got(_Newl)
		return wi
	}
	p.got(_Newl)
	if pos, ok := p.gotRsrv("in"); ok {
		wi.InPos = pos
		for !p.stopToken() {
			if w := p.getWord(); w == nil {
				p.curErr("word list can only contain words")
			} else {
				wi.Items = append(wi.Items, w)
			}
		}
		p.got(semicolon)
		p.got(_Newl)
	} else if p.tok == _LitWord && p.val == "do" {
	} else {
		p.followErr(fpos, ftok+" foo", noQuote("`in`, `do`, `;`, or a newline"))
	}
	return wi
}

func (p *Parser) selectClause(s *Stmt) {
	fc := &ForClause{ForPos: p.pos, Select: true}
	p.next()
	fc.Loop = p.wordIter("select", fc.ForPos)
	fc.DoPos = p.followRsrv(fc.ForPos, "select foo [in words]", "do")
	fc.Do, fc.DoLast = p.followStmts("do", fc.DoPos, "done")
	fc.DonePos = p.stmtEnd(fc, "select", "done")
	s.Cmd = fc
}

func (p *Parser) caseClause(s *Stmt) {
	cc := &CaseClause{Case: p.pos}
	p.next()
	cc.Word = p.getWord()
	if cc.Word == nil {
		p.followErr(cc.Case, "case", noQuote("a word"))
	}
	end := "esac"
	p.got(_Newl)
	if pos, ok := p.gotRsrv("{"); ok {
		cc.In = pos
		cc.Braces = true
		p.checkLang(cc.Pos(), LangMirBSDKorn, "`case i {`")
		end = "}"
	} else {
		cc.In = p.followRsrv(cc.Case, "case x", "in")
	}
	cc.Items = p.caseItems(end)
	cc.Last, p.accComs = p.accComs, nil
	cc.Esac = p.stmtEnd(cc, "case", end)
	s.Cmd = cc
}

func (p *Parser) caseItems(stop string) (items []*CaseItem) {
	p.got(_Newl)
	for p.tok != _EOF && (p.tok != _LitWord || p.val != stop) {
		ci := &CaseItem{}
		ci.Comments, p.accComs = p.accComs, nil
		p.got(leftParen)
		for p.tok != _EOF {
			if w := p.getWord(); w == nil {
				p.curErr("case patterns must consist of words")
			} else {
				ci.Patterns = append(ci.Patterns, w)
			}
			if p.tok == rightParen {
				break
			}
			if !p.got(or) {
				p.curErr("case patterns must be separated with %#q", or)
			}
		}
		old := p.preNested(switchCase)
		p.next()
		ci.Stmts, ci.Last = p.stmtList(stop)
		p.postNested(old)
		switch p.tok {
		case dblSemicolon, semiAnd, dblSemiAnd, semiOr:
		default:
			ci.Op = Break
			items = append(items, ci)
			return items
		}
		ci.Last = append(ci.Last, p.accComs...)
		p.accComs = nil
		ci.OpPos = p.pos
		ci.Op = CaseOperator(p.tok)
		p.next()
		p.got(_Newl)

		// Split the comments:
		//
		// case x in
		// a)
		//   foo
		//   ;;
		//   # comment for a
		// # comment for b
		// b)
		//   [...]
		split := len(p.accComs)
		for i, c := range slices.Backward(p.accComs) {
			if c.Pos().Col() != p.pos.Col() {
				break
			}
			split = i
		}
		ci.Comments = append(ci.Comments, p.accComs[:split]...)
		p.accComs = p.accComs[split:]

		items = append(items, ci)
	}
	return items
}

func (p *Parser) testClause(s *Stmt) {
	tc := &TestClause{Left: p.pos}
	old := p.preNested(testExpr)
	p.next()
	if tc.X = p.testExprBinary(false); tc.X == nil {
		p.followErrExp(tc.Left, dblLeftBrack)
	}
	tc.Right = p.pos
	if _, ok := p.gotRsrv("]]"); !ok {
		p.matchingErr(tc.Left, dblLeftBrack, dblRightBrack)
	}
	p.postNested(old)
	s.Cmd = tc
}

func (p *Parser) testExprBinary(pastAndOr bool) TestExpr {
	p.got(_Newl)
	var left TestExpr
	if pastAndOr {
		left = p.testExprUnary()
	} else {
		left = p.testExprBinary(true)
	}
	if left == nil {
		return left
	}
	p.got(_Newl)
	switch p.tok {
	case andAnd, orOr:
	case _LitWord:
		if p.val == "]]" {
			return left
		}
		if p.tok = token(testBinaryOp(p.val)); p.tok == illegalTok {
			p.curErr("not a valid test operator: %#q", p.val)
		}
	case rdrIn, rdrOut:
	case _EOF, rightParen:
		return left
	case _Lit:
		p.curErr("test operator words must consist of a single literal")
	default:
		p.curErr("not a valid test operator: %#q", p.tok)
	}
	b := &BinaryTest{
		OpPos: p.pos,
		Op:    BinTestOperator(p.tok),
		X:     left,
	}
	switch b.Op {
	case AndTest, OrTest:
		p.next()
		if b.Y = p.testExprBinary(false); b.Y == nil {
			p.followErrExp(b.OpPos, b.Op)
		}
	case TsReMatch:
		p.checkLang(p.pos, langBashLike|LangZsh, "regex tests")
		p.rxOpenParens = 0
		p.rxFirstPart = true
		// TODO(mvdan): Using nested states within a regex will break in
		// all sorts of ways. The better fix is likely to use a stop
		// token, like we do with heredocs.
		p.quote = testExprRegexp
		fallthrough
	default:
		if _, ok := b.X.(*Word); !ok {
			p.posErr(b.OpPos, "expected %#q, %#q or %#q after complex expr",
				AndTest, OrTest, dblRightBrack)
		}
		p.next()
		b.Y = p.followWordTok(token(b.Op), b.OpPos)
	}
	return b
}

func (p *Parser) testExprUnary() TestExpr {
	switch p.tok {
	case _EOF, rightParen:
		return nil
	case _LitWord:
		op := token(testUnaryOp(p.val))
		switch op {
		case illegalTok:
		case tsRefVar, tsModif: // not available in mksh
			if p.lang.in(langBashLike) {
				p.tok = op
			}
		default:
			p.tok = op
		}
	}
	switch p.tok {
	case exclMark:
		u := &UnaryTest{OpPos: p.pos, Op: TsNot}
		p.next()
		if u.X = p.testExprBinary(false); u.X == nil {
			p.followErrExp(u.OpPos, u.Op)
		}
		return u
	case tsExists, tsRegFile, tsDirect, tsCharSp, tsBlckSp, tsNmPipe,
		tsSocket, tsSmbLink, tsSticky, tsGIDSet, tsUIDSet, tsGrpOwn,
		tsUsrOwn, tsModif, tsRead, tsWrite, tsExec, tsNoEmpty,
		tsFdTerm, tsEmpStr, tsNempStr, tsOptSet, tsVarSet, tsRefVar:
		u := &UnaryTest{OpPos: p.pos, Op: UnTestOperator(p.tok)}
		p.next()
		u.X = p.followWordTok(token(u.Op), u.OpPos)
		return u
	case leftParen:
		pe := &ParenTest{Lparen: p.pos}
		p.next()
		if pe.X = p.testExprBinary(false); pe.X == nil {
			p.followErrExp(pe.Lparen, leftParen)
		}
		pe.Rparen = p.matched(pe.Lparen, leftParen, rightParen)
		return pe
	case _LitWord:
		if p.val == "]]" {
			return nil
		}
		fallthrough
	default:
		if w := p.getWord(); w != nil {
			return w
		}
		// otherwise we'd return a typed nil above
		return nil
	}
}

func (p *Parser) declClause(s *Stmt) {
	ds := &DeclClause{Variant: p.lit(p.pos, p.val)}
	p.next()
	for !p.stopToken() && !p.peekRedir() {
		if p.hasValidIdent() {
			ds.Args = append(ds.Args, p.getAssign(false))
		} else if p.tok.isLit() && p.eqlOffs > 0 && !strings.Contains(p.val[:p.eqlOffs], "{") {
			p.curErr("invalid var name")
		} else if p.tok == _LitWord && ValidName(p.val) {
			ds.Args = append(ds.Args, &Assign{
				Naked: true,
				Name:  p.getLit(),
			})
		} else if w := p.getWord(); w != nil {
			ds.Args = append(ds.Args, &Assign{
				Naked: true,
				Value: w,
			})
		} else {
			p.followErr(p.pos, ds.Variant.Value, noQuote("names or assignments"))
		}
	}
	s.Cmd = ds
}

func isBashCompoundCommand(tok token, val string) bool {
	switch tok {
	case leftParen, dblLeftParen:
		return true
	case _LitWord:
		switch val {
		case "{", "if", "while", "until", "for", "case", "[[",
			"coproc", "let", "function", "declare", "local",
			"export", "readonly", "typeset", "nameref":
			return true
		}
	}
	return false
}

func (p *Parser) timeClause(s *Stmt) {
	tc := &TimeClause{Time: p.pos}
	p.next()
	if _, ok := p.gotRsrv("-p"); ok {
		tc.PosixFormat = true
	}
	tc.Stmt = p.gotStmtPipe(&Stmt{Position: p.pos}, false)
	s.Cmd = tc
}

func (p *Parser) coprocClause(s *Stmt) {
	cc := &CoprocClause{Coproc: p.pos}
	if p.next(); isBashCompoundCommand(p.tok, p.val) {
		// has no name
		cc.Stmt = p.gotStmtPipe(&Stmt{Position: p.pos}, false)
		s.Cmd = cc
		return
	}
	cc.Name = p.getWord()
	cc.Stmt = p.gotStmtPipe(&Stmt{Position: p.pos}, false)
	if cc.Stmt == nil {
		if cc.Name == nil {
			p.posErr(cc.Coproc, "coproc clause requires a command")
			return
		}
		// name was in fact the stmt
		cc.Stmt = &Stmt{Position: cc.Name.Pos()}
		cc.Stmt.Cmd = p.call(cc.Name)
		cc.Name = nil
	} else if cc.Name != nil {
		if call, ok := cc.Stmt.Cmd.(*CallExpr); ok {
			// name was in fact the start of a call
			call.Args = append([]*Word{cc.Name}, call.Args...)
			cc.Name = nil
		}
	}
	s.Cmd = cc
}

func (p *Parser) letClause(s *Stmt) {
	lc := &LetClause{Let: p.pos}
	old := p.preNested(arithmExprLet)
	p.next()
	for !p.stopToken() && !p.peekRedir() {
		x := p.arithmExpr(true)
		if x == nil {
			break
		}
		lc.Exprs = append(lc.Exprs, x)
	}
	if len(lc.Exprs) == 0 && !p.stopToken() && !p.peekRedir() {
		// Something is there and it is not an expression, which bash
		// also calls a syntax error. A bare `let` is different: bash
		// parses it and the *builtin* answers `let: expression
		// expected` with status 1, so the empty clause is kept and the
		// interpreter reports it (#593).
		p.followErrExp(lc.Let, "let")
	}
	p.postNested(old)
	s.Cmd = lc
}

func (p *Parser) bashFuncDecl(s *Stmt) {
	fpos := p.pos
	p.next()
	names := make([]*Lit, 0, 1)
	for p.tok == _LitWord && p.val != "{" {
		names = append(names, p.lit(p.pos, p.val))
		p.next()
	}
	hasParens := p.got(leftParen)
	switch len(names) {
	case 0:
		if hasParens || (p.tok == _LitWord && p.val == "{") {
			p.checkLang(fpos, LangZsh, "anonymous functions")
		} else if !p.lang.in(LangZsh) {
			p.followErr(fpos, "function", noQuote("a name"))
		}
		names = nil // avoid non-nil zero-length slices
	case 1:
		// allowed in all variants
	default:
		p.checkLang(fpos, LangZsh, "multi-name functions")
	}
	if hasParens {
		p.follow(fpos, "function foo(", rightParen)
	}
	p.funcDecl(s, fpos, true, hasParens, names...)
}

func (p *Parser) testDecl(s *Stmt) {
	td := &TestDecl{Position: p.pos}
	p.next()
	if td.Description = p.getWord(); td.Description == nil {
		p.followErr(td.Position, "@test", noQuote("a description word"))
	}
	if td.Body = p.getStmt(false, false, true); td.Body == nil {
		p.followErr(td.Position, `@test "desc"`, noQuote("a statement"))
	}
	s.Cmd = td
}

func (p *Parser) unexpectedInCallExpr(ce *CallExpr) {
	// Note that we'll only keep the first error that happens.
	if len(ce.Args) > 0 {
		if cmd := ce.Args[0].Lit(); isBashCompoundCommand(_LitWord, cmd) {
			p.checkLang(p.pos, langBashLike, "the %#q builtin", cmd)
		}
	}
	p.curErr("a command can only contain words and redirects; encountered %#q", p.tok)
}

func (p *Parser) callExpr(s *Stmt, w *Word, assign bool) {
	ce := p.call(w)
	if w == nil {
		ce.Args = ce.Args[:0]
	}
	if assign {
		ce.Assigns = append(ce.Assigns, p.getAssign(true))
	}
loop:
	for {
		switch p.tok {
		case _EOF, _Newl, semicolon, and, or, andAnd, orOr, orAnd, andPipe, andBang,
			dblSemicolon, semiAnd, dblSemiAnd, semiOr:
			break loop
		case _LitWord:
			if len(ce.Args) == 0 && p.hasValidIdent() {
				ce.Assigns = append(ce.Assigns, p.getAssign(true))
				break
			}
			// Avoid failing later with the confusing "} can only be used to close a block".
			if p.val == "{" && w != nil && w.Lit() == "function" {
				p.checkLang(p.pos, langBashLike, `the "function" builtin`)
			}
			// Zsh does not require a semicolon to close a block.
			if p.lang.in(LangZsh) && p.val == "}" {
				break loop
			}
			w := p.wordOne(p.lit(p.pos, p.val))
			p.next()
			if p.lang.in(LangZsh) && !p.spaced {
				w.Parts = append(w.Parts, p.wordParts(nil)...)
			}
			ce.Args = append(ce.Args, w)
		case _Lit:
			if len(ce.Args) == 0 && p.hasValidIdent() {
				ce.Assigns = append(ce.Assigns, p.getAssign(true))
				break
			}
			ce.Args = append(ce.Args, p.wordAnyNumber())
		case bckQuote:
			if p.backquoteEnd() {
				break loop
			}
			fallthrough
		case dollBrace, dollDblParen, dollParen, dollar, cmdIn, assgnParen, cmdOut,
			sglQuote, dollSglQuote, dblQuote, dollDblQuote, dollBrack,
			globQuest, globStar, globPlus, globAt, globExcl:
			ce.Args = append(ce.Args, p.wordAnyNumber())
		case dblLeftParen:
			p.curErr("%#q can only be used to open an arithmetic cmd", p.tok)
		case leftParen:
			if p.lang.in(LangZsh) && p.r != ')' {
				ce.Args = append(ce.Args, p.wordAnyNumber())
				break
			}
			p.unexpectedInCallExpr(ce)
		case rightParen:
			if p.quote == subCmd {
				break loop
			}
			p.unexpectedInCallExpr(ce)
		default:
			if p.peekRedir() {
				p.doRedirect(s)
				continue
			}
			p.unexpectedInCallExpr(ce)
		}
	}
	if len(ce.Args) == 0 {
		ce.Args = nil
	} else {
		for _, asgn := range ce.Assigns {
			if asgn.Index != nil || asgn.Array != nil {
				p.posErr(asgn.Pos(), "inline variables cannot be arrays")
			}
		}
	}
	s.Cmd = ce
}

func (p *Parser) funcDecl(s *Stmt, pos Pos, long, withParens bool, names ...*Lit) {
	fd := &FuncDecl{
		Position: pos,
		RsrvWord: long,
		Parens:   withParens,
	}
	if len(names) == 1 {
		fd.Name = names[0]
	} else {
		fd.Names = names
	}
	p.got(_Newl)
	// TODO: reject any body which isn't a compound command, like a quoted word
	if fd.Body = p.getStmt(false, false, true); fd.Body == nil {
		p.followErr(fd.Pos(), "foo()", noQuote("a statement"))
	}
	s.Cmd = fd
}
