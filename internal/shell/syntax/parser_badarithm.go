package syntax

import "unicode/utf8"

// arithmBailout is what a parse error inside an arithmetic expression
// panics with, so that the construct reading it regains control with
// the lexer still alive. bash parses arithmetic when it evaluates it,
// from a string, so malformed arithmetic is a runtime error there and
// koi — which parses ahead of what it runs — forfeited the rest of the
// file for it (#600). Recovered only by [Parser.arithmRead]; anything
// else panicking through is re-raised untouched.
type arithmBailout struct{}

// arithmEnd names the delimiter that closes an arithmetic construct,
// which is what the scan after a bailout looks for.
type arithmEnd int

const (
	// arithmEndDblParen is the `))` of `$(( … ))` and `(( … ))`.
	arithmEndDblParen arithmEnd = iota
	// arithmEndSemi is one part of a C-style loop's header: a `;`, or
	// the `))` when it is the last part.
	arithmEndSemi
	// arithmEndBrack is the `]` of `$[ … ]`.
	arithmEndBrack
	// arithmEndBrace is the `}` of a `${x:off:len}` slice, whose halves
	// are the one arithmetic that lives inside an expansion.
	arithmEndBrace
)

// beginRaw starts recording source bytes for an arithmetic construct,
// so a bailout can name the expression as written. Regions nest — a
// `$(( … ))` inside a C-style loop's header — and share one recording,
// since every byte between them passes through [Parser.rune] in order
// and a position therefore indexes straight into it.
//
// It must be called before the construct's first token is lexed, since
// the lexer reads one rune ahead: the rune it is sitting on is the
// first of the expression, and is seeded here the way [Parser.newLit]
// seeds a literal.
func (p *Parser) beginRaw() {
	if p.arithmRawDepth == 0 {
		p.arithmRaw = p.arithmRaw[:0]
		p.arithmRawOff = int64(p.nextPos().Offset())
		switch r := p.r; {
		case r == runeEOF || r == escNewl:
			// Sentinel runes not present in the input as-is. An escaped
			// newline is two or three real bytes and seeding it would
			// take a guess at which, so a construct that *begins* on
			// one falls through to the disagreement check below and
			// keeps the parse error; one crossed part-way through is
			// recorded and printed back exactly.
		case r < utf8.RuneSelf:
			p.arithmRaw = append(p.arithmRaw, byte(r))
		default:
			w := uint(utf8.RuneLen(r))
			p.arithmRaw = append(p.arithmRaw, p.bs[p.bsp-w:p.bsp]...)
		}
	}
	p.arithmRawDepth++
}

// endRaw stops recording once the outermost construct is done with it.
func (p *Parser) endRaw() {
	if p.arithmRawDepth--; p.arithmRawDepth <= 0 {
		p.arithmRawDepth = 0
		p.arithmRaw = p.arithmRaw[:0]
	}
}

// arithmRead parses an arithmetic expression the way bash reads one:
// text first, verdict later. What parse cannot read becomes a
// [*BadArithm] holding the source between the construct's delimiters,
// which the evaluator refuses when it runs (#600) — in a word that
// abandons the input unit, and in `(( ))`, a C-style loop's header or
// `let` it is only that command failing, which is the split #597
// measured.
//
// startPos is where the expression's source begins; the zero Pos means
// the rune the lexer is sitting on, for a region whose first token has
// not been lexed yet. end names the delimiter the scan looks for.
//
// Only bash-like languages have a runtime verdict to defer to, so the
// others keep the parse error they always had.
func (p *Parser) arithmRead(startPos Pos, end arithmEnd, parse func() ArithmExpr) (x ArithmExpr) {
	if !p.lang.in(langBashLike) {
		return parse()
	}
	p.beginRaw()
	if !startPos.IsValid() {
		startPos = p.nextPos()
	}
	p.arithmBails++
	defer func() {
		p.arithmBails--
		if r := recover(); r != nil {
			if _, ok := r.(arithmBailout); !ok {
				p.endRaw()
				panic(r)
			}
			x = p.badArithm(startPos, end)
		}
		p.endRaw()
	}()
	return parse()
}

// atArithmEnd reports whether the parser is holding the delimiter that
// closes the construct, which is where a well-formed expression leaves
// it.
func (p *Parser) atArithmEnd(end arithmEnd) bool {
	switch end {
	case arithmEndSemi:
		return p.tok == semicolon || p.tok == dblSemicolon || p.peekArithmEnd()
	case arithmEndBrack:
		return p.tok == rightBrack
	case arithmEndBrace:
		return p.tok == rightBrace
	}
	return p.peekArithmEnd()
}

// arithmTail reports what the construct's own delimiter check is about
// to report, while the expression's marker can still absorb it. Text
// left over where an arithmetic construct should have ended is bash's
// runtime complaint like any other — `$(( a b c ))`, `$(( a ; c ))` —
// and only the tokens *inside* an expression reach a bailout on their
// own, so this is the one place the tail can raise it (#600).
//
// It stands aside for the [RecoverErrors] option, whose invented tokens
// are the delimiter check's own answer and would otherwise never be
// reached.
func (p *Parser) arithmTail(end arithmEnd, lpos Pos, ltok token) {
	if p.atArithmEnd(end) || p.canRecoverError() {
		return
	}
	right := dblRightParen
	if end == arithmEndBrack {
		right = rightBrack
	}
	p.arithmMatchingErr(lpos, ltok, right)
}

// badArithm finishes a construct whose expression bailed out: it finds
// the delimiter that ends the expression and answers with the text
// between, positioned where it was written.
//
// The delimiter may already have been lexed, since a parse gives up one
// token late as often as not — `(( 4 + ))` reports the missing operand
// with the `)` in hand — so the scan starts from the *recording* rather
// than from where the lexer stands: the bailout unwound past whatever
// brackets the expression had opened, and re-reading the source it
// consumed is what recovers their depth.
//
// The scan can find nothing to stop at — the input ran out, which is an
// error in bash too — and then the error the parse would have reported
// stands, as it does if the recording and the positions ever disagree.
func (p *Parser) badArithm(startPos Pos, end arithmEnd) ArithmExpr {
	p.litBs = nil // a literal the bailed-out parse was midway through
	base := int(p.arithmRawOff)
	i := int(startPos.Offset()) - base
	if i < 0 || i > len(p.arithmRaw) {
		p.stopErr(p.bailErr)
		return nil
	}
	sc := arithmScan{end: end}
	stop, found := sc.advance(p.arithmRaw[i:], p.peek())
	if found && i+stop < int(p.nextPos().Offset())-base {
		// The delimiter is behind the lexer: it is the token the parse
		// was holding when it gave up, so nothing more needs reading.
		if i+stop != int(p.pos.Offset())-base {
			p.stopErr(p.bailErr)
			return nil
		}
		return &BadArithm{ValuePos: startPos, ValueEnd: p.pos, Value: string(p.arithmRaw[i : i+stop])}
	}
	outer := p.quote
	p.quote = runeByRune
	for !found && p.r != runeEOF {
		p.rune()
		stop, found = sc.advance(p.arithmRaw[i:], p.peek())
	}
	p.quote = outer
	if !found || i+stop != int(p.nextPos().Offset())-base {
		p.stopErr(p.bailErr)
		return nil
	}
	endPos := p.nextPos()
	p.next() // the delimiter, which the caller reads as its own
	return &BadArithm{ValuePos: startPos, ValueEnd: endPos, Value: string(p.arithmRaw[i : i+stop])}
}

// arithmScan finds the delimiter that ends an arithmetic construct in
// raw source. Only the quotes, a backslash, a backquote and the nesting
// brackets are significant, for the same reason they are the only
// things significant to [Parser.subscriptText] and [Parser.braceText]:
// bash is finding where a construct ends rather than reading what is
// inside it.
type arithmScan struct {
	end   arithmEnd
	i     int  // how far into the text the scan has read
	depth int  // open brackets
	quote byte // the quote the scan is inside, if any
}

// advance reads text from where it left off, reporting the index of the
// delimiter if it is in there. tailPeek is the byte following the text,
// which a `))` needs to be recognized at the very end of it.
func (s *arithmScan) advance(text []byte, tailPeek byte) (int, bool) {
	for ; s.i < len(text); s.i++ {
		b := text[s.i]
		if s.quote != 0 {
			if b == '\\' && s.quote != '\'' {
				s.i++
			} else if b == s.quote {
				s.quote = 0
			}
			continue
		}
		peek := tailPeek
		if s.i+1 < len(text) {
			peek = text[s.i+1]
		}
		switch b {
		case '\\':
			s.i++ // whatever follows is literal, including a `)`
		case '\'', '"', '`':
			s.quote = b
		case '(', '[', '{':
			s.depth++
		case ']':
			if s.depth == 0 && s.end == arithmEndBrack {
				return s.i, true
			}
			if s.depth > 0 {
				s.depth--
			}
		case '}':
			if s.depth == 0 && s.end == arithmEndBrace {
				return s.i, true
			}
			if s.depth > 0 {
				s.depth--
			}
		case ';':
			if s.depth == 0 && s.end == arithmEndSemi {
				return s.i, true
			}
		case ')':
			if s.depth > 0 {
				s.depth--
			} else if s.end != arithmEndBrack && s.end != arithmEndBrace && peek == ')' {
				return s.i, true
			}
		}
	}
	return 0, false
}
