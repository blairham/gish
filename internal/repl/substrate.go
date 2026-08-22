package repl

import (
	"github.com/blairham/koi-shell/internal/shell/syntax"
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
// `declare -F` / `typeset -F` was the founding entry: it parses as a
// *declaration clause*, which the interpreter implements itself and which
// no handler observes (the same wall declcall.go documents from the other
// side), and it is how agent harnesses and init scripts enumerate a shell
// — Claude Code's shell snapshot carries the user's functions across with
// it. The substrate refused it then and implements it now, so the rewrite
// is gone (#615), and the reason is the one `>|` records below rather than
// mere tidiness: a rewrite is equivalent only until the substrate learns a
// distinction the rewrite was flattening, and here the distinction is the
// attribute. `readonly -f f` makes `declare -F` report `declare -fr f`,
// which a shim reading only the function *table* cannot know — so keeping
// it would have shadowed the correct answer with a stale one on exactly
// the path a snapshot takes. Two capabilities came back with the
// deletion, both of which the scope note below had written off: a
// subshell's own functions, and `declare -F` inside a `source` or an
// `eval`.
//
// A quoted heredoc delimiter was a second entry here, restating the body
// as the literal its delimiter promised (#244). It is gone because the
// scope note below was the whole problem: rewriting the tree koi parses
// cannot reach a heredoc inside `source` or `eval`, which re-parse in the
// interpreter, so the corruption stayed live on the two paths a script is
// most likely to take. Owning interp and expand (#272) turned that from
// an upstream wait into an edit, and it is now fixed where it belongs, in
// interp.literalHdoc (#259).
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
// `eval` re-parse inside the interpreter, so a construct rewritten here
// is still the substrate's answer inside a sourced file (#242). Closing
// that means fixing it in the substrate, which is where every entry this
// file has ever had has ended up.

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
		}
		return true
	})
}
