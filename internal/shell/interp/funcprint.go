package interp

import (
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// The canonical function printer (#386).
//
// `declare -f`, `type` and `declare -p -f` exist so a function can be
// read back — by a person, by a diff, by a script that captures the
// definition and re-evaluates it elsewhere. bash does not print the
// source text it read: it re-renders the parse tree in one fixed shape,
// which is why two functions that differ only in whitespace print
// identically. koi printed the mvdan/sh printer's shape instead, so
// every function listing differed from bash's even where the semantics
// agreed — cprint.tests is a whole file of exactly that comparison.
//
// The layout below is bash's, established by running bash 5.3 rather
// than read out of print_cmd.c:
//
//	name () <space>
//	{ <space>
//	    stmt;
//	    stmt
//	}
//
// Four-space indent per level; every statement in a block terminated by
// a semicolon except the last; a trailing space after the `()` and
// after the opening brace, which is why the shapes above are written
// with explicit markers. Three normalizations are bash's own and are
// reproduced because they change the text: `elif` renders as a nested
// `else if`, a nested function declaration gains the `function`
// keyword, and a duplicating redirection grows its default descriptor
// (`>&2` prints as `1>&2`).

// funcPrinter renders one function body in bash's canonical shape.
type funcPrinter struct {
	sb    strings.Builder
	depth int
	// wp renders leaf nodes — words, conditions, arithmetic — where
	// bash prints the same text the parser recorded. Only the statement
	// *layout* differs between the two printers, so leaves are
	// delegated rather than reimplemented.
	wp *syntax.Printer
}

// printFuncCanonical renders `name` and `body` the way bash's declare -f
// does, including the trailing newline.
func printFuncCanonical(name string, body *syntax.Stmt, keyword bool) string {
	p := &funcPrinter{wp: syntax.NewPrinter(syntax.SingleLine(true))}
	if keyword {
		p.sb.WriteString("function ")
	}
	fmt.Fprintf(&p.sb, "%s () \n", name)
	p.block(body)
	p.sb.WriteString("\n")
	return p.sb.String()
}

// block renders a function body: the `{ … }` wrapper plus its
// statements. The body of a function is always a block in bash's
// output, even when the source wrote a subshell.
func (p *funcPrinter) block(body *syntax.Stmt) {
	stmts := []*syntax.Stmt{body}
	switch cmd := body.Cmd.(type) {
	case *syntax.Block:
		stmts = cmd.Stmts
	case *syntax.Subshell:
		// `f() ( … )` keeps its subshell, printed on one line.
		p.indent()
		p.stmt(body)
		p.sb.WriteString("\n")
		return
	}
	p.sb.WriteString("{ \n")
	p.depth++
	p.stmts(stmts, true)
	p.depth--
	p.sb.WriteString("}")
}

// stmts renders a run of statements, one per line. Every one is
// terminated by a semicolon except the last statement of the function's
// own top-level block: inside a compound body bash terminates all of
// them, so `if …; then echo a; fi` keeps the semicolon after `echo a`
// and drops it only after the outermost `fi`.
func (p *funcPrinter) stmts(stmts []*syntax.Stmt, bareLast bool) {
	for i, st := range stmts {
		p.indent()
		p.stmt(st)
		// A background statement is already terminated by its `&`, and
		// a semicolon after it does not re-parse — which is the one
		// thing this printer exists to guarantee.
		if !st.Background && (!bareLast || i < len(stmts)-1) {
			p.sb.WriteString(";")
		}
		p.sb.WriteString("\n")
	}
}

func (p *funcPrinter) indent() {
	p.sb.WriteString(strings.Repeat("    ", p.depth))
}

// stmt renders one statement without its terminator.
func (p *funcPrinter) stmt(st *syntax.Stmt) {
	if st.Negated {
		p.sb.WriteString("! ")
	}
	p.cmd(st.Cmd)
	for _, rd := range st.Redirs {
		p.sb.WriteString(" ")
		p.redir(rd)
	}
	if st.Background {
		p.sb.WriteString(" &")
	}
}

// redir renders a redirection, giving a duplicating form the default
// descriptor bash prints: `>&2` becomes `1>&2`.
func (p *funcPrinter) redir(rd *syntax.Redirect) {
	if rd.N != nil {
		p.sb.WriteString(rd.N.Value)
	} else {
		switch rd.Op {
		case syntax.DplOut:
			p.sb.WriteString("1")
		case syntax.DplIn:
			p.sb.WriteString("0")
		}
	}
	p.sb.WriteString(rd.Op.String())
	switch rd.Op {
	case syntax.DplOut, syntax.DplIn:
		// No space: `1>&2`, not `1>& 2`.
	default:
		p.sb.WriteString(" ")
	}
	if rd.Hdoc != nil {
		p.word(rd.Word)
		return
	}
	p.word(rd.Word)
}

// word renders a single word, part by part, so an arithmetic expansion
// inside it takes the compact spelling arithm gives it rather than the
// delegate printer's spaced one.
func (p *funcPrinter) word(w *syntax.Word) {
	if w == nil {
		return
	}
	for _, part := range w.Parts {
		if ae, ok := part.(*syntax.ArithmExp); ok && !ae.Bracket {
			p.sb.WriteString("$((")
			p.arithm(ae.X)
			p.sb.WriteString("))")
			continue
		}
		p.leaf(part)
	}
}

// assign renders `name=value`, routing the value through word so an
// arithmetic expansion keeps its compact spelling.
func (p *funcPrinter) assign(as *syntax.Assign) {
	if as.Name == nil {
		p.leaf(as)
		return
	}
	p.sb.WriteString(as.Name.Value)
	if as.Index != nil {
		p.sb.WriteString("[")
		p.arithm(as.Index)
		p.sb.WriteString("]")
	}
	if as.Naked {
		return
	}
	if as.Append {
		p.sb.WriteString("+")
	}
	p.sb.WriteString("=")
	if as.Array != nil {
		p.sb.WriteString("(")
		for i, el := range as.Array.Elems {
			if i > 0 {
				p.sb.WriteString(" ")
			}
			if el.Index != nil {
				p.sb.WriteString("[")
				p.arithm(el.Index)
				p.sb.WriteString("]=")
			}
			p.word(el.Value)
		}
		p.sb.WriteString(")")
		return
	}
	p.word(as.Value)
}

// leaf renders any node through the delegate printer, for the pieces
// whose text bash prints unchanged.
func (p *funcPrinter) leaf(node syntax.Node) {
	var sb strings.Builder
	p.wp.Print(&sb, node) //nolint:errcheck // writing to a strings.Builder
	p.sb.WriteString(sb.String())
}

// inline renders a statement list on one line, as a condition: `a; b`.
func (p *funcPrinter) inline(stmts []*syntax.Stmt) {
	for i, st := range stmts {
		if i > 0 {
			p.sb.WriteString("; ")
		}
		p.stmt(st)
	}
}

// body renders an indented statement list between a header and a
// closing keyword.
func (p *funcPrinter) body(stmts []*syntax.Stmt) {
	p.depth++
	p.stmts(stmts, false)
	p.depth--
	p.indent()
}

func (p *funcPrinter) cmd(cmd syntax.Command) {
	switch c := cmd.(type) {
	case *syntax.CallExpr:
		first := true
		for _, as := range c.Assigns {
			if !first {
				p.sb.WriteString(" ")
			}
			p.assign(as)
			first = false
		}
		for _, w := range c.Args {
			if !first {
				p.sb.WriteString(" ")
			}
			p.word(w)
			first = false
		}
	case *syntax.Block:
		p.sb.WriteString("{ \n")
		p.depth++
		p.stmts(c.Stmts, true)
		p.depth--
		p.indent()
		p.sb.WriteString("}")
	case *syntax.Subshell:
		p.sb.WriteString("( ")
		p.inline(c.Stmts)
		p.sb.WriteString(" )")
	case *syntax.IfClause:
		p.ifClause(c, "if")
	case *syntax.WhileClause:
		word := "while"
		if c.Until {
			word = "until"
		}
		p.sb.WriteString(word + " ")
		p.inline(c.Cond)
		p.sb.WriteString("; do\n")
		p.body(c.Do)
		p.sb.WriteString("done")
	case *syntax.ForClause:
		// for and select put `do` on its own line, where while and
		// until keep it on the header's — bash's inconsistency, kept.
		word := "for "
		if c.Select {
			word = "select "
		}
		p.sb.WriteString(word)
		switch loop := c.Loop.(type) {
		case *syntax.WordIter:
			p.sb.WriteString(loop.Name.Value)
			if len(loop.Items) > 0 {
				p.sb.WriteString(" in")
				for _, w := range loop.Items {
					p.sb.WriteString(" ")
					p.word(w)
				}
			}
			p.sb.WriteString(";")
		case *syntax.CStyleLoop:
			// The C-style header carries no terminator before `do`.
			p.sb.WriteString("((")
			p.arithm(loop.Init)
			p.sb.WriteString("; ")
			p.arithm(loop.Cond)
			p.sb.WriteString("; ")
			p.arithm(loop.Post)
			p.sb.WriteString("))")
		}
		p.sb.WriteString("\n")
		p.indent()
		p.sb.WriteString("do\n")
		p.body(c.Do)
		p.sb.WriteString("done")
	case *syntax.CaseClause:
		p.sb.WriteString("case ")
		p.word(c.Word)
		p.sb.WriteString(" in \n")
		p.depth++
		for _, item := range c.Items {
			p.indent()
			for i, pat := range item.Patterns {
				if i > 0 {
					p.sb.WriteString(" | ")
				}
				p.word(pat)
			}
			p.sb.WriteString(")\n")
			p.depth++
			p.stmts(item.Stmts, true)
			p.depth--
			p.indent()
			p.sb.WriteString(item.Op.String() + "\n")
		}
		p.depth--
		p.indent()
		p.sb.WriteString("esac")
	case *syntax.BinaryCmd:
		p.stmt(c.X)
		p.sb.WriteString(" " + c.Op.String() + " ")
		p.stmt(c.Y)
	case *syntax.FuncDecl:
		// A nested declaration gains bash's `function` keyword.
		s := printFuncCanonical(c.Name.Value, c.Body, true)
		s = strings.TrimSuffix(s, "\n")
		p.sb.WriteString(indentAfterFirst(s, p.depth))
	default:
		// Arithmetic commands, [[ ]] tests, let, coproc and the rest
		// print the same text in both printers.
		p.leaf(cmd)
	}
}

// ifClause renders an if, expanding elif into the nested else-if bash
// prints.
func (p *funcPrinter) ifClause(c *syntax.IfClause, word string) {
	p.sb.WriteString(word + " ")
	p.inline(c.Cond)
	p.sb.WriteString("; then\n")
	p.body(c.Then)
	if c.Else == nil {
		p.sb.WriteString("fi")
		return
	}
	if elifClause, ok := elseIsElif(c.Else); ok {
		p.sb.WriteString("else\n")
		p.depth++
		p.indent()
		p.ifClause(elifClause, "if")
		p.sb.WriteString(";\n")
		p.depth--
		p.indent()
		p.sb.WriteString("fi")
		return
	}
	p.sb.WriteString("else\n")
	p.body(c.Else.Then)
	p.sb.WriteString("fi")
}

// elseIsElif reports whether an else branch is really an elif, which
// the parser records as an IfClause with no condition of its own.
func elseIsElif(el *syntax.IfClause) (*syntax.IfClause, bool) {
	if len(el.Cond) == 0 {
		return nil, false
	}
	return el, true
}

// indentAfterFirst re-indents every line but the first, for a nested
// rendering that already carries its own internal indentation.
func indentAfterFirst(s string, depth int) string {
	pad := strings.Repeat("    ", depth)
	lines := strings.Split(s, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

// arithm renders an arithmetic expression compactly — `i<2`, not
// `i < 2`. bash echoes arithmetic *verbatim from the source it read*,
// which is the one thing a printer working from a parse tree cannot
// reproduce: `$(( 1  +  2 ))` keeps its double spaces there. Compact is
// the spelling scripts actually write, so it is what matches most
// often; the residual is stated rather than hidden.
func (p *funcPrinter) arithm(x syntax.ArithmExpr) {
	switch e := x.(type) {
	case nil:
	case *syntax.Word:
		p.word(e)
	case *syntax.BinaryArithm:
		p.arithm(e.X)
		p.sb.WriteString(e.Op.String())
		p.arithm(e.Y)
	case *syntax.UnaryArithm:
		if e.Post {
			p.arithm(e.X)
			p.sb.WriteString(e.Op.String())
			return
		}
		p.sb.WriteString(e.Op.String())
		p.arithm(e.X)
	case *syntax.ParenArithm:
		p.sb.WriteString("(")
		p.arithm(e.X)
		p.sb.WriteString(")")
	default:
		p.leaf(x)
	}
}

// The printer's contract is that its output re-parses to the same
// function. printFuncCanonicalRoundTrips is the seam a test drives to
// assert it, which is stronger than comparing text: a shape that only
// *looks* right is exactly what a semicolon after `&` produces.
func printFuncCanonicalRoundTrips(name string, body *syntax.Stmt) error {
	out := printFuncCanonical(name, body, false)
	_, err := syntax.NewParser().Parse(strings.NewReader(out), "")
	return err
}
