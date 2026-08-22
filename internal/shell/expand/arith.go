// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package expand

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/syntax"
)

// TODO(v4): the arithmetic APIs should return int64 for portability with 32-bit systems,
// even if Bash only supports native int sizes.

// ArithError marks a diagnostic the arithmetic evaluator produced
// itself, as against one raised by an expansion *inside* the
// expression. The distinction is bash's, and it is about control flow
// rather than wording (#597): in a word — `echo $((5 += 2))` — an
// arithmetic error abandons the command and the rest of the line, while
// `(( 5 += 2 ))` and `let 5+=2` are commands whose evaluation failed, so
// they report under their own name, answer 1, and let the line carry on.
// A bad substitution inside `(( ))` still abandons the line, which is
// why the mark sits on the arithmetic errors rather than on the context
// that evaluates them.
type ArithError struct{ Err error }

func (e ArithError) Error() string { return e.Err.Error() }

func (e ArithError) Unwrap() error { return e.Err }

// aerrf is [fmt.Errorf] for the evaluator's own diagnostics.
func aerrf(format string, args ...any) error {
	return ArithError{Err: fmt.Errorf(format, args...)}
}

// arithmWordStr evaluates the string a word in arithmetic context
// expanded to, the way bash does: a name reads its value and evaluates
// *that*, a number is itself, and anything else re-parses as a whole
// arithmetic expression — `y=1+1; echo $((y))` answers 2, and
// `let "x = 5+2"` assigns through the quotes (#367). A string that does
// not parse is an arithmetic syntax error rather than a silent zero,
// which is how a garbage ${var:offset} used to slice from 0 (#366).
func (cfg *Config) arithmWordStr(str string) (int, error) {
	str = strings.TrimSpace(str)
	if str == "" {
		return 0, nil
	}
	// A plain name reads its value and evaluates that; a chase that
	// dead-ends answers 0 while one that cycles is an error, as 5.3's.
	for i := 0; syntax.ValidName(str); i++ {
		if i >= maxNameRefDepth {
			return 0, aerrf("%s: expression recursion level exceeded", str)
		}
		str = strings.TrimSpace(cfg.envGet(str))
		if str == "" {
			return 0, nil
		}
	}
	if n, ok := atoiStrict(str); ok {
		return int(n), nil
	}
	// Anything else re-parses as a whole arithmetic expression, so
	// y=1+1 makes $((y)) answer 2 and let "x = 5+2" assigns through the
	// quotes (#367); a string that does not parse — or parses back to
	// itself, like a bare `}` — is an arithmetic syntax error rather
	// than a silent zero (#366).
	if cfg.arithStrDepth++; cfg.arithStrDepth > 128 {
		cfg.arithStrDepth--
		return 0, aerrf("%s: expression recursion level exceeded", str)
	}
	defer func() { cfg.arithStrDepth-- }()
	expr, err := syntax.NewParser().Arithmetic(strings.NewReader(str))
	if err != nil {
		return 0, aerrf("%s: arithmetic syntax error", str)
	}
	if expr == nil {
		return 0, nil
	}
	if w, ok := expr.(*syntax.Word); ok && strings.TrimSpace(w.Lit()) == str {
		return 0, aerrf("%s: arithmetic syntax error", str)
	}
	return Arithm(cfg, expr)
}

// atoiStrict is [atoi] with the validity reported instead of swallowed:
// it accepts exactly the numeric spellings bash's arithmetic does —
// optional sign, 0x hex, leading-0 octal, and base#digits up to 64 —
// and reports false for anything else instead of answering 0.
func atoiStrict(s string) (int64, bool) {
	neg := false
	switch {
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	case strings.HasPrefix(s, "-"):
		neg = true
		s = s[1:]
	}
	base := int64(10)
	switch {
	case strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X"):
		base = 16
		s = s[2:]
	case s == "0":
		return 0, true
	case strings.HasPrefix(s, "0"):
		base = 8
		s = s[1:]
	default:
		baseStr, intStr, hasSep := strings.Cut(s, "#")
		if hasSep {
			var err error
			base, err = strconv.ParseInt(baseStr, 10, 8)
			if err != nil || base < 2 || base > 64 {
				return 0, false
			}
			s = intStr
		}
	}
	if s == "" {
		return 0, false
	}
	var n int64
	if base > 36 {
		var ok bool
		if n, ok = atoiLargeBaseStrict(s, base); !ok {
			return 0, false
		}
	} else {
		var err error
		if n, err = strconv.ParseInt(s, int(base), 64); err != nil {
			return 0, false
		}
	}
	if neg {
		n = -n
	}
	return n, true
}

// atoiLargeBaseStrict is [atoiLargeBase] with invalid digits reported.
func atoiLargeBaseStrict(s string, base int64) (int64, bool) {
	var n int64
	for i := range len(s) {
		var d int64
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case c >= 'a' && c <= 'z':
			d = int64(c-'a') + 10
		case c >= 'A' && c <= 'Z':
			d = int64(c-'A') + 36
		case c == '@':
			d = 62
		case c == '_':
			d = 63
		default:
			return 0, false
		}
		if d >= base {
			return 0, false
		}
		n = n*base + d
	}
	return n, true
}

func Arithm(cfg *Config, expr syntax.ArithmExpr) (int, error) {
	if expr == nil {
		return 0, nil // the empty `$(())`, which bash evaluates as zero
	}
	switch expr := expr.(type) {
	case *syntax.Word:
		str, err := Literal(cfg, expr)
		if err != nil {
			return 0, err
		}
		return cfg.arithmWordStr(str)
	case *syntax.ParenArithm:
		return Arithm(cfg, expr.X)
	case *syntax.UnaryArithm:
		switch expr.Op {
		case syntax.Inc, syntax.Dec:
			target, err := cfg.arithTarget(expr.X)
			if err != nil {
				return 0, err
			}
			cur, err := cfg.readTarget(target)
			if err != nil {
				return 0, err
			}
			old := atoi(cur)
			val := old
			if expr.Op == syntax.Inc {
				val++
			} else {
				val--
			}
			if err := cfg.writeTarget(target, strconv.FormatInt(val, 10)); err != nil {
				return 0, err
			}
			if expr.Post {
				return int(old), nil
			}
			return int(val), nil
		}
		val, err := Arithm(cfg, expr.X)
		if err != nil {
			return 0, err
		}
		switch expr.Op {
		case syntax.Not:
			return oneIf(val == 0), nil
		case syntax.BitNegation:
			return ^val, nil
		case syntax.Plus:
			return val, nil
		case syntax.Minus:
			return -val, nil
		default:
			return 0, aerrf("unsupported unary arithmetic operator: %q", expr.Op)
		}
	case *syntax.BinaryArithm:
		switch expr.Op {
		case syntax.Assgn, syntax.AddAssgn, syntax.SubAssgn,
			syntax.MulAssgn, syntax.QuoAssgn, syntax.RemAssgn,
			syntax.AndAssgn, syntax.OrAssgn, syntax.XorAssgn,
			syntax.ShlAssgn, syntax.ShrAssgn:
			return cfg.assgnArit(expr)
		case syntax.TernQuest: // TernColon can't happen here
			cond, err := Arithm(cfg, expr.X)
			if err != nil {
				return 0, err
			}
			b2 := expr.Y.(*syntax.BinaryArithm) // must have Op==TernColon
			if cond != 0 {
				return Arithm(cfg, b2.X)
			}
			return Arithm(cfg, b2.Y)
		case syntax.AndArit, syntax.OrArit:
			// Like Bash, short-circuit the right operand.
			left, err := Arithm(cfg, expr.X)
			if err != nil {
				return 0, err
			}
			if expr.Op == syntax.AndArit && left == 0 {
				return 0, nil
			}
			if expr.Op == syntax.OrArit && left != 0 {
				return 1, nil
			}
			right, err := Arithm(cfg, expr.Y)
			if err != nil {
				return 0, err
			}
			return oneIf(right != 0), nil
		}
		left, err := Arithm(cfg, expr.X)
		if err != nil {
			return 0, err
		}
		right, err := Arithm(cfg, expr.Y)
		if err != nil {
			return 0, err
		}
		return binArit(expr.Op, left, right)
	case *syntax.FlagsArithm: // e.g. zsh's ${a[(r)b]}
		return 0, fmt.Errorf("unsupported")
	default:
		panic(fmt.Sprintf("unexpected arithm expr: %T", expr))
	}
}

func oneIf(b bool) int {
	if b {
		return 1
	}
	return 0
}

// atoi is like [strconv.ParseInt](s, BASE, 64), but it handles integer
// base prefixes according to bash-shell's rules, ignores errors, and
// trims whitespace.
//
// For more information about bash's integer base handling syntax,
// refer to the bash manual:
// https://www.man7.org/linux/man-pages/man1/bash.1.html
func atoi(s string) int64 {
	s = strings.TrimSpace(s)
	neg := false
	switch {
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	case strings.HasPrefix(s, "-"):
		neg = true
		s = s[1:]
	}
	base := int64(10)
	switch {
	case strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X"):
		base = 16
		s = s[2:]
	case strings.HasPrefix(s, "0"):
		base = 8
		s = s[1:]
	default:
		baseStr, intStr, hasSep := strings.Cut(s, "#")
		if hasSep {
			var err error
			base, err = strconv.ParseInt(baseStr, 10, 8)
			if err != nil || base < 2 || base > 64 {
				return 0
			}
			s = intStr
		}
	}
	var n int64
	if base > 36 {
		n = atoiLargeBase(s, base)
	} else {
		n, _ = strconv.ParseInt(s, int(base), 64)
	}
	if neg {
		n = -n
	}
	return n
}

// atoiLargeBase parses bases 37 to 64, which [strconv.ParseInt] does not
// support, using bash's digit set: 0-9, a-z, A-Z, "@", and "_".
func atoiLargeBase(s string, base int64) int64 {
	var n int64
	for i := range len(s) {
		var d int64
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case c >= 'a' && c <= 'z':
			d = int64(c-'a') + 10
		case c >= 'A' && c <= 'Z':
			d = int64(c-'A') + 36
		case c == '@':
			d = 62
		case c == '_':
			d = 63
		default:
			return 0
		}
		if d >= base {
			return 0
		}
		n = n*base + d
	}
	return n
}

func (cfg *Config) assgnArit(b *syntax.BinaryArithm) (int, error) {
	target, err := cfg.arithTarget(b.X)
	if err == nil && !target.assignable() {
		err = errNotAssignable
	}
	if errors.Is(err, errNotAssignable) {
		// bash's wording, and its shape: the whole assignment is named
		// and the operator with its right-hand side is the "error
		// token" (#597). koi's spacing is the printer's rather than the
		// source's, which is the one part that can differ.
		return 0, aerrf("%s: attempted assignment to non-variable (error token is %q)",
			nodeText(b), b.Op.String()+" "+nodeText(b.Y))
	}
	if err != nil {
		// Something else went wrong deciding what the target *is* — an
		// expansion inside it failed — which is its own diagnostic
		// rather than a verdict about the target.
		return 0, err
	}
	cur, err := cfg.readTarget(target)
	if err != nil {
		return 0, err
	}
	val := atoi(cur)
	arg_, err := Arithm(cfg, b.Y)
	if err != nil {
		return 0, err
	}
	arg := int64(arg_)
	switch b.Op {
	case syntax.Assgn:
		val = arg
	case syntax.AddAssgn:
		val += arg
	case syntax.SubAssgn:
		val -= arg
	case syntax.MulAssgn:
		val *= arg
	case syntax.QuoAssgn:
		if arg == 0 {
			return 0, aerrf("division by zero")
		}
		val /= arg
	case syntax.RemAssgn:
		if arg == 0 {
			return 0, aerrf("division by zero")
		}
		val %= arg
	case syntax.AndAssgn:
		val &= arg
	case syntax.OrAssgn:
		val |= arg
	case syntax.XorAssgn:
		val ^= arg
	case syntax.ShlAssgn:
		val <<= uint(arg)
	case syntax.ShrAssgn:
		val >>= uint(arg)
	}
	if err := cfg.writeTarget(target, strconv.FormatInt(val, 10)); err != nil {
		return 0, err
	}
	return int(val), nil
}

func intPow(a, b int) int {
	p := 1
	for b > 0 {
		if b&1 != 0 {
			p *= a
		}
		b >>= 1
		a *= a
	}
	return p
}

func binArit(op syntax.BinAritOperator, x, y int) (int, error) {
	switch op {
	case syntax.Add:
		return x + y, nil
	case syntax.Sub:
		return x - y, nil
	case syntax.Mul:
		return x * y, nil
	case syntax.Quo:
		if y == 0 {
			return 0, aerrf("division by zero")
		}
		return x / y, nil
	case syntax.Rem:
		if y == 0 {
			return 0, aerrf("division by zero")
		}
		return x % y, nil
	case syntax.Pow:
		if y < 0 {
			return 0, aerrf("exponent less than 0")
		}
		return intPow(x, y), nil
	case syntax.Eql:
		return oneIf(x == y), nil
	case syntax.Gtr:
		return oneIf(x > y), nil
	case syntax.Lss:
		return oneIf(x < y), nil
	case syntax.Neq:
		return oneIf(x != y), nil
	case syntax.Leq:
		return oneIf(x <= y), nil
	case syntax.Geq:
		return oneIf(x >= y), nil
	case syntax.And:
		return x & y, nil
	case syntax.Or:
		return x | y, nil
	case syntax.Xor:
		return x ^ y, nil
	case syntax.Shr:
		return x >> uint(y), nil
	case syntax.Shl:
		return x << uint(y), nil
	case syntax.Comma:
		// x is executed but its result discarded
		return y, nil
	default:
		return 0, aerrf("unsupported binary arithmetic operator: %q", op)
	}
}

// arithTarget is what an arithmetic assignment writes to: a variable,
// and an element of it when the target is subscripted (#277).
//
// Two shapes reach here that a bare literal name does not cover, and
// both used to be refused or to panic. `a[i]=9` names an element, which
// is read and written like any other element rather than through the
// variable's string form — that one answered "variable name must not be
// empty", a koi bug rather than a diagnosis. And a name can be
// *computed*: bash expands the word first, so `${v}ame=1` and `x$$=1`
// assign to whatever the expansion spells, which is how bash's own
// new-exp.tests writes `${_ENV[(_$-=0)+(_=1)]}`.
type arithTarget struct {
	name  string
	index syntax.ArithmExpr // nil for a plain variable
}

// errNotAssignable is "this is not somewhere a value can be stored",
// which only an *assignment* cares about: `++5` reaches the same target
// machinery and bash answers it differently, so the verdict is the
// caller's to word (#597).
var errNotAssignable = errors.New("not an assignment target")

// assignable reports whether an assignment may write here. An element
// is always assignable — the array is created if it has to be — and a
// plain variable has to be a name, which for a computed target is only
// knowable once the word has been expanded.
func (t arithTarget) assignable() bool {
	return t.index != nil || syntax.ValidName(t.name)
}

func (cfg *Config) arithTarget(x syntax.ArithmExpr) (arithTarget, error) {
	w, ok := x.(*syntax.Word)
	if !ok {
		// Anything that is not a word is not a place at all: `(a)+=b`
		// and `0 && B=42`, where assignment binds looser than `&&` so
		// the target is the whole left side.
		return arithTarget{}, errNotAssignable
	}
	if len(w.Parts) == 1 {
		switch part := w.Parts[0].(type) {
		case *syntax.Lit:
			return cfg.followNameRefTarget(arithTarget{name: part.Value}), nil
		case *syntax.ParamExp:
			// `a[i]` reaches the parser as a short parameter expansion
			// with an index and nothing else; anything more is a value
			// rather than a target, and the parser has already refused
			// it as one.
			if part.Short && part.Index != nil && part.Param != nil {
				return arithTarget{name: part.Param.Value, index: part.Index}, nil
			}
		}
	}
	// A word with expansions in it: what it spells is the name, which is
	// only knowable now.
	name, err := Literal(cfg, w)
	if err != nil {
		return arithTarget{}, err
	}
	return cfg.followNameRefTarget(arithTarget{name: name}), nil
}

// followNameRefTarget resolves a reference before an arithmetic
// assignment writes through it (#610): `declare -n r=v; (( r = 20 ))`
// sets **v**, where writing the name unseen made r an ordinary variable
// and left v alone. A reference may also name an element, which becomes
// a subscripted target the same way a parameter expansion's does.
func (cfg *Config) followNameRefTarget(t arithTarget) arithTarget {
	if t.index != nil {
		return t
	}
	vr := cfg.Env.Get(t.name)
	if vr.Kind != NameRef {
		return t
	}
	if base, sub, ok := cutNameRefSubscript(vr.Str); ok {
		if idx := subscriptWord(sub); idx != nil {
			return arithTarget{name: base, index: idx}
		}
	}
	if n, _ := vr.Resolve(cfg.Env); n != "" {
		t.name = n
	}
	return t
}

// readTarget is the target's current value, empty when it is unset.
func (cfg *Config) readTarget(t arithTarget) (string, error) {
	if t.index == nil {
		return cfg.envGet(t.name), nil
	}
	str, _, err := cfg.varInd(cfg.Env.Get(t.name), t.index)
	return str, err
}

// writeTarget stores a value in the target, creating the array when the
// target is an element of one — `a[9]=7` on an unset `a` is an array
// with one element in bash, not an error.
func (cfg *Config) writeTarget(t arithTarget, val string) error {
	if t.index == nil {
		if !syntax.ValidName(t.name) {
			// bash reads an unusable name as a *number* and complains
			// about that, since in its grammar the left side of an
			// assignment is just another operand.
			return aerrf("%s: not a valid name", t.name)
		}
		return cfg.envSet(t.name, val)
	}
	return cfg.assignElem(t.name, cfg.Env.Get(t.name), t.index, val)
}
