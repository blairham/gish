package repl

import (
	"context"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// `\export FOO=bar` — a declaration reached as a call (#119).
//
// The parser turns `export`, `local`, `readonly`, `declare` and
// `typeset` into a *declaration clause*, and the interpreter implements
// them there. Escape or quote the name — `\export`, `"export"`,
// `command export` — and the parse changes: it is an ordinary command
// call, the builtin table has no entry for it, and the shell answers
// "export: unsupported builtin" while **setting nothing**.
//
// That spelling is not exotic. Defensive init scripts write it
// deliberately to bypass an alias or a function of the same name, and
// conda's `shell.bash hook` does exactly that with both `\export` and
// `\local` — which is how CI found this: the ubuntu runner has conda
// installed, the source gate (#161) loaded it, and every variable the
// hook exported came back empty behind three lines of stderr.
//
// Silent and wrong is the worst failure available here. The message goes
// to stderr, the script keeps running, and the variables it believes it
// set are empty.
//
// The fix routes the call back to the declaration form the interpreter
// does implement, by rewriting it to `eval`. Assignments are re-quoted
// on the way through, so a value with spaces or quotes in it arrives
// whole; anything that is not an assignment is left for the interpreter
// to reject, because `\export -f fn` is a real thing to refuse and
// pretending to support it would be a worse answer than the current one.

// declBuiltins are the declaration keywords that only work as clauses.
var declBuiltins = map[string]bool{
	"export":   true,
	"local":    true,
	"readonly": true,
	"declare":  true,
	"typeset":  true,
}

// declCallHandler rewrites an escaped declaration into its clause form.
func declCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if len(args) < 2 || !declBuiltins[args[0]] {
			return next(ctx, args)
		}
		src, ok := declSource(args)
		if !ok {
			return next(ctx, args)
		}
		// eval runs in the caller's scope, which is what makes `local`
		// still local: a subshell would set the variable somewhere the
		// function cannot see.
		return []string{"eval", src}, nil
	}
}

// declSource renders the clause to re-run, or ok=false when the call is
// not purely assignments and bare names.
func declSource(args []string) (string, bool) {
	var b strings.Builder
	b.WriteString(args[0])
	for _, arg := range args[1:] {
		name, value, isAssign := strings.Cut(arg, "=")
		switch {
		case strings.HasPrefix(arg, "-"):
			// An option (-f, -n, -p, -a) changes what the clause means,
			// and guessing is worse than declining.
			return "", false
		case !isAssign:
			if !validName(name) {
				return "", false
			}
			b.WriteString(" " + name)
		case !validName(name):
			return "", false
		default:
			quoted, err := syntax.Quote(value, syntax.LangBash)
			if err != nil {
				return "", false
			}
			b.WriteString(" " + name + "=" + quoted)
		}
	}
	return b.String(), true
}

// validName reports whether s is a shell variable name — the guard that
// keeps this from re-running something that only looked like one.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
