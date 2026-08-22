// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package interp

import (
	"fmt"

	"github.com/blairham/koi-shell/internal/shell/syntax"
)

const illegalTok = 0

type testParser struct {
	eof bool
	val string
	rem []string

	err func(err error)
}

func (p *testParser) errf(format string, a ...any) {
	p.err(fmt.Errorf(format, a...))
}

func (p *testParser) next() {
	if p.eof || len(p.rem) == 0 {
		p.eof = true
		p.val = ""
		return
	}
	p.val = p.rem[0]
	p.rem = p.rem[1:]
}

func (p *testParser) followWord(fval string) *syntax.Word {
	if p.eof {
		p.errf("%s must be followed by a word", fval)
	}
	w := &syntax.Word{Parts: []syntax.WordPart{
		&syntax.Lit{Value: p.val},
	}}
	p.next()
	return w
}

// classicOr parses the loosest level of POSIX `test`'s grammar: a chain
// of `-o` over `-a` chains. bash's test.c is written that way -- or()
// calls and(), which calls term() -- so `a -a b -o c` is `(a -a b) -o
// c`. Giving the two equal precedence read it as `a -a (b -o c)` and
// answered the wrong branch with nothing printed (#669).
func (p *testParser) classicOr(fval string) syntax.TestExpr {
	left := p.classicAnd(fval)
	if left == nil || p.eof || p.val == ")" || testBinaryOp(p.val) != syntax.OrTest {
		return left
	}
	b := &syntax.BinaryTest{Op: syntax.OrTest, X: left}
	opStr := p.val
	p.next()
	if b.Y = p.classicOr(opStr); b.Y == nil {
		p.errf("%s must be followed by an expression", opStr)
	}
	return b
}

// classicAnd parses a chain of `-a` over terms, which binds tighter
// than `-o`.
func (p *testParser) classicAnd(fval string) syntax.TestExpr {
	left := p.classicTerm(fval)
	if left == nil || p.eof || p.val == ")" || testBinaryOp(p.val) != syntax.AndTest {
		return left
	}
	b := &syntax.BinaryTest{Op: syntax.AndTest, X: left}
	opStr := p.val
	p.next()
	if b.Y = p.classicAnd(opStr); b.Y == nil {
		p.errf("%s must be followed by an expression", opStr)
	}
	return b
}

// classicTerm parses one term -- bash's term(): a `!`, a parenthesized
// expression, a unary operator and its word, or a word possibly
// followed by a binary comparison.
func (p *testParser) classicTerm(fval string) syntax.TestExpr {
	left := p.testExprBase(fval)
	if left == nil || p.eof || p.val == ")" {
		return left
	}
	opStr := p.val
	op := testBinaryOp(p.val)
	switch op {
	case illegalTok:
		p.errf("not a valid test operator: %#q", p.val)
	case syntax.AndTest, syntax.OrTest:
		return left
	}
	b := &syntax.BinaryTest{
		Op: op,
		X:  left,
	}
	p.next()
	b.Y = p.followWord(opStr)
	return b
}

func (p *testParser) testExprBase(fval string) syntax.TestExpr {
	if p.eof || p.val == ")" {
		return nil
	}
	op := testUnaryOp(p.val)
	switch op {
	case syntax.TsNot:
		u := &syntax.UnaryTest{Op: op}
		p.next()
		// bash's term() negates one term, so `! a -a b` is
		// `(!a) -a b` -- see classicOr (#669).
		u.X = p.classicTerm(op.String())
		return u
	case syntax.TsParen:
		pe := &syntax.ParenTest{}
		p.next()
		pe.X = p.classicOr(op.String())
		if p.val != ")" {
			p.errf("reached %s without matching '(' with ')'", p.val)
		}
		p.next()
		return pe
	case illegalTok:
		return p.followWord(fval)
	default:
		u := &syntax.UnaryTest{Op: op}
		p.next()
		if p.eof {
			// make [ -e ] fall back to [ -n -e ], i.e. use
			// the operator as an argument
			return &syntax.Word{Parts: []syntax.WordPart{
				&syntax.Lit{Value: op.String()},
			}}
		}
		u.X = p.followWord(op.String())
		return u
	}
}

// testUnaryOp is an exact copy of syntax's.
func testUnaryOp(val string) syntax.UnTestOperator {
	switch val {
	case "!":
		return syntax.TsNot
	case "(":
		return syntax.TsParen
	case "-e", "-a":
		return syntax.TsExists
	case "-f":
		return syntax.TsRegFile
	case "-d":
		return syntax.TsDirect
	case "-c":
		return syntax.TsCharSp
	case "-b":
		return syntax.TsBlckSp
	case "-p":
		return syntax.TsNmPipe
	case "-S":
		return syntax.TsSocket
	case "-L", "-h":
		return syntax.TsSmbLink
	case "-k":
		return syntax.TsSticky
	case "-g":
		return syntax.TsGIDSet
	case "-u":
		return syntax.TsUIDSet
	case "-G":
		return syntax.TsGrpOwn
	case "-O":
		return syntax.TsUsrOwn
	case "-N":
		return syntax.TsModif
	case "-r":
		return syntax.TsRead
	case "-w":
		return syntax.TsWrite
	case "-x":
		return syntax.TsExec
	case "-s":
		return syntax.TsNoEmpty
	case "-t":
		return syntax.TsFdTerm
	case "-z":
		return syntax.TsEmpStr
	case "-n":
		return syntax.TsNempStr
	case "-o":
		return syntax.TsOptSet
	case "-v":
		return syntax.TsVarSet
	case "-R":
		return syntax.TsRefVar
	default:
		return illegalTok
	}
}

// testBinaryOp is like syntax's, but with -a and -o, and without =~.
func testBinaryOp(val string) syntax.BinTestOperator {
	switch val {
	case "-a":
		return syntax.AndTest
	case "-o":
		return syntax.OrTest
	case "==", "=":
		return syntax.TsMatch
	case "!=":
		return syntax.TsNoMatch
	case "-nt":
		return syntax.TsNewer
	case "-ot":
		return syntax.TsOlder
	case "-ef":
		return syntax.TsDevIno
	case "-eq":
		return syntax.TsEql
	case "-ne":
		return syntax.TsNeq
	case "-le":
		return syntax.TsLeq
	case "-ge":
		return syntax.TsGeq
	case "-lt":
		return syntax.TsLss
	case "-gt":
		return syntax.TsGtr
	case "<":
		// POSIX leaves these to the XSI string comparison; bash
		// implements them in test and [ and koi called them invalid
		// operators (#401), which turned an ordinary sort check into a
		// status 2.
		return syntax.TsBefore
	case ">":
		return syntax.TsAfter
	default:
		return illegalTok
	}
}
