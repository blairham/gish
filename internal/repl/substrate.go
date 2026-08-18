package repl

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/builtins"
)

// Local fixes for substrate gaps the handler seams cannot reach (#119).
//
// The usual route for a construct the interpreter refuses is the
// CallHandler: rename the call so it arrives somewhere koi controls
// (overrides.go). Some constructs never become a call at all — the
// *parser* classifies them and the interpreter handles them inside its
// own dispatch, so no handler ever observes them — and the only seam
// left is the tree between parse and run:
//
//   - `declare -F` / `typeset -F` parse as a *declaration clause*, which
//     the interpreter implements itself and which no handler observes
//     (the same wall declcall.go documents from the other side).
//   - A **quoted** heredoc delimiter is supposed to make the body
//     entirely literal, and the escape processing happens during
//     expansion — after the delimiter's quoting is gone — so
//     `cat > f <<'EOF'` writes a file with backslashes missing from it.
//     There is no call and no redirect op to rename here: the fix is to
//     restate the body as the literal it already is.
//
// The first is how agent harnesses and init scripts enumerate a shell:
// Claude Code's shell snapshot carries the user's functions across with
// `declare -F`, so a shell that fails it is one those tools cannot drive
// at all.
//
// `>|` was a third entry here, rewritten to a plain `>`. It is now
// implemented in the substrate and the rewrite had to go, because the
// same carry implements `set -C`: renaming `>|` to `>` took the one
// redirect which is *supposed* to ignore noclobber and turned it into the
// one form noclobber refuses. That is worse than the gap the rename was
// covering, and it is the argument against this file growing rather than
// shrinking — a rewrite is equivalent only until the substrate learns the
// distinction it was flattening. The corpus case "`>|` overrides
// noclobber" exists to keep it gone.
//
// Scope, stated rather than discovered later: this rewrites what *koi*
// parses — an interactive line, `-c`, and a script file. `source` and
// `eval` re-parse inside the interpreter, so a `declare -F` in a sourced
// file is still the substrate's answer (#242). Closing that means fixing
// it in the substrate, which is what #119 tracks; this is the part koi
// can carry meanwhile.

// clobberEquivalent maps each clobbering redirect to the plain form the
// interpreter implements. These parse only under [syntax.LangZsh], which koi
// never selects, so they are unreachable today and are kept so that a future
// dialect switch does not reintroduce the silent failure `>|` used to have.
//
// `>|` ([syntax.RdrClob]) is deliberately absent: the substrate implements it,
// and renaming it now would make noclobber refuse it.
//
// Note that only the appending forms are safe to rename, for the same reason.
// If a zsh dialect is ever selected, `&>|` ([syntax.RdrAllClob]) has to be
// dropped from here and fixed in the substrate the way `>|` was, because `&>`
// is refused under noclobber whereas `&>|` must not be. Appending is never
// refused, so `>>|` and `&>>|` are unaffected either way.
var clobberEquivalent = map[syntax.RedirOperator]syntax.RedirOperator{
	syntax.AppClob:    syntax.AppOut,
	syntax.RdrAllClob: syntax.RdrAll,
	syntax.AppAllClob: syntax.AppAll,
}

// rewriteSubstrateGaps rewrites the constructs above, in place.
func rewriteSubstrateGaps(node syntax.Node) {
	syntax.Walk(node, func(n syntax.Node) bool {
		stmt, ok := n.(*syntax.Stmt)
		if !ok {
			return true
		}
		for _, rd := range stmt.Redirs {
			// Noclobber has landed in the substrate, so a clobbering form
			// and its plain counterpart no longer mean the same thing.
			// See the map for which renames survive that, and why.
			if plain, ok := clobberEquivalent[rd.Op]; ok {
				rd.Op = plain
			}
			literalizeHdoc(rd)
		}
		if call, ok := declFuncQuery(stmt.Cmd); ok {
			stmt.Cmd = call
		}
		return true
	})
}

// declFuncQuery converts `declare -F [name…]` into a call to the native
// builtin, or reports false for every other declaration clause.
//
// Deliberately narrow: only the -F query, only when every argument is a
// bare word. `declare -F` with an assignment in it is not a query, and
// guessing at a mixed clause would be worse than the interpreter's
// current refusal.
func declFuncQuery(cmd syntax.Command) (*syntax.CallExpr, bool) {
	decl, ok := cmd.(*syntax.DeclClause)
	if !ok || decl.Variant == nil {
		return nil, false
	}
	if decl.Variant.Value != "declare" && decl.Variant.Value != "typeset" {
		return nil, false
	}
	names := make([]*syntax.Word, 0, len(decl.Args))
	sawF := false
	for _, arg := range decl.Args {
		if !arg.Naked || arg.Index != nil || arg.Array != nil {
			return nil, false
		}
		// A bare name arrives as a naked assign with no value — the
		// parser has already told the two apart, and `declare -F name`
		// is a name, not an option.
		if arg.Value == nil {
			if arg.Name == nil {
				return nil, false
			}
			names = append(names, litWord(arg.Name.Value))
			continue
		}
		lit, isLit := soleLit(arg.Value)
		switch {
		case isLit && lit == "-F":
			sawF = true
		case isLit && (strings.HasPrefix(lit, "-") || strings.HasPrefix(lit, "+")):
			// Any other option changes what the clause means.
			return nil, false
		default:
			names = append(names, arg.Value)
		}
	}
	if !sawF {
		return nil, false
	}
	return &syntax.CallExpr{Args: append([]*syntax.Word{litWord(declFuncsName)}, names...)}, true
}

// soleLit returns the word's value when it is a single unquoted
// literal — the only shape an option can take.
func soleLit(w *syntax.Word) (string, bool) {
	if len(w.Parts) != 1 {
		return "", false
	}
	lit, ok := w.Parts[0].(*syntax.Lit)
	if !ok {
		return "", false
	}
	return lit.Value, true
}

func litWord(s string) *syntax.Word {
	return &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: s}}}
}

// declFuncsName is the registry name the rewrite dispatches to; the
// __koi_ prefix keeps it out of the user-facing builtin listing, like
// every other rewritten name.
const declFuncsName = "__koi_declare_funcs"

// declareFuncs answers `declare -F`, reading the session runner's own
// function table.
//
// bash's two output shapes are not a detail — the callers depend on
// them. Bare `declare -F` prints `declare -f NAME` per function (which
// is why harnesses pipe it through `cut -d' ' -f3`), while `declare -F
// name` prints the bare name and exits 1 when it is not a function,
// because that form is a test.
//
// Functions defined in a subshell are not visible here: the interpreter
// gives a subshell its own copy of the table and this reads the
// session's. Enumerating a shell from inside a subshell it will discard
// is not a real use, and the alternative is threading a runner through
// the exec seam.
func declareFuncs(_ context.Context, hc interp.HandlerContext, args []string) error {
	runner := sessionRunner()
	if runner == nil {
		fmt.Fprintln(hc.Stderr, "declare: no session to query")
		return interp.ExitStatus(1)
	}
	if len(args) == 0 {
		defined := make([]string, 0, len(runner.Funcs))
		for name := range runner.Funcs {
			defined = append(defined, name)
		}
		slices.Sort(defined)
		for _, name := range defined {
			fmt.Fprintf(hc.Stdout, "declare -f %s\n", name)
		}
		return nil
	}
	missing := false
	for _, name := range args {
		if _, ok := runner.Funcs[name]; !ok {
			missing = true
			continue
		}
		fmt.Fprintln(hc.Stdout, name)
	}
	if missing {
		return interp.ExitStatus(1)
	}
	return nil
}

// registerSubstrateBuiltins wires the names the rewrite dispatches to.
// Called from every session path, since a script asks these questions
// as readily as an interactive line does.
//
// Once per process, not once per session: the builtin holds no session
// state (it reads whichever runner is current), and the registry is a
// package-level map that the test binary would otherwise have several
// sessions writing at the same time.
func registerSubstrateBuiltins() {
	substrateBuiltinsOnce.Do(func() {
		builtins.Register(declFuncsName, declareFuncs)
	})
}

var substrateBuiltinsOnce sync.Once

// literalizeHdoc restates a quoted-delimiter heredoc body as a
// single-quoted part, so it survives expansion unchanged (#244).
//
// POSIX is unambiguous: "if any character in word is quoted, the
// here-document lines shall not be expanded". The substrate gets the
// expansion half right — `$HOME`, `$(cmd)`, backquotes and `$((1+1))`
// all stay literal under `<<'X'` and all expand under `<<X`, matching
// bash exactly — and gets escapes wrong, because escapes are processed
// during expansion and by then the delimiter's quoting is gone.
// expand.Document runs one backslash pass for both forms, stripping the
// `\` before `\`, `$` and a backquote. That is precisely the *unquoted*
// heredoc's rule, applied to the quoted one.
//
// `cat > file <<'EOF'` is the idiom for writing a file whose content
// must not be interpreted — the quoted delimiter is the whole reason it
// is safe — so the damage is a wrong artifact rather than a refusal. A
// regex spelled `\\d`, a Windows path, a Makefile recipe, a printf
// format or an escaped `\$` lands on disk with a backslash missing, no
// message, and exit 0. It is the shape #215 described for `>|`: the
// silence is the expensive part, except here the file exists and reads
// as plausible.
//
// Restating the body as [syntax.SglQuoted] says what the delimiter's
// quoting means, and it deliberately is not a re-escaping of the
// current escape set: doubling every backslash would be an exact
// inverse of today's pass in expand.go and would become a silent lie
// the moment that pass changes.
func literalizeHdoc(rd *syntax.Redirect) {
	if rd.Hdoc == nil || !hdocDelimQuoted(rd.Word) {
		return
	}
	// The quoted delimiter sends the lexer down its own path, which
	// yields exactly one literal and no expansion tree to preserve. Any
	// other shape means the parser did *not* agree the delimiter was
	// quoted, and rewriting it would turn live expansions into text —
	// so leave it alone and keep the substrate's answer.
	if len(rd.Hdoc.Parts) != 1 {
		return
	}
	lit, ok := rd.Hdoc.Parts[0].(*syntax.Lit)
	if !ok {
		return
	}
	// `<<-'X'` strips leading tabs from every line, and that pass runs in
	// the interpreter over the *literal* parts — so restating the body as
	// a quoted part moves it out of reach and the tabs would survive.
	// Measured, not assumed: the first cut of this fix traded the eaten
	// backslash for kept tabs, which is how a fix becomes a different
	// bug. Do the stripping here instead, since after this the body is
	// final text rather than something left to expand.
	value := lit.Value
	if rd.Op == syntax.DashHdoc {
		lines := strings.Split(value, "\n")
		for i, line := range lines {
			lines[i] = strings.TrimLeft(line, "\t")
		}
		value = strings.Join(lines, "\n")
	}
	rd.Hdoc.Parts[0] = &syntax.SglQuoted{
		Left:  lit.ValuePos,
		Right: lit.ValueEnd,
		Value: value,
	}
}

// hdocDelimQuoted reports whether the *parser* treated this heredoc's
// delimiter as quoted.
//
// It matches syntax.Parser.unquotedWordBytes rather than POSIX, quirk
// included: that function overwrites its verdict per part instead of
// accumulating it, so only the last part decides and `<<'X'Y` is read as
// unquoted (#258). Matching is the point rather than an oversight — this
// decision is what picked the lexer path that built the body, and being
// more correct than the parser here would mean rewriting a tree that
// still has real expansions in it.
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
