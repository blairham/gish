// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package expand

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/syntax"
)

// TODO(v4): the arithmetic APIs should return int64 for portability with 32-bit systems,
// even if Bash only supports native int sizes.

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
			return 0, fmt.Errorf("%s: expression recursion level exceeded", str)
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
		return 0, fmt.Errorf("%s: expression recursion level exceeded", str)
	}
	defer func() { cfg.arithStrDepth-- }()
	expr, err := syntax.NewParser().Arithmetic(strings.NewReader(str))
	if err != nil {
		return 0, fmt.Errorf("%s: arithmetic syntax error", str)
	}
	if expr == nil {
		return 0, nil
	}
	if w, ok := expr.(*syntax.Word); ok && strings.TrimSpace(w.Lit()) == str {
		return 0, fmt.Errorf("%s: arithmetic syntax error", str)
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
			name := expr.X.(*syntax.Word).Lit()
			old := atoi(cfg.envGet(name))
			val := old
			if expr.Op == syntax.Inc {
				val++
			} else {
				val--
			}
			if err := cfg.envSet(name, strconv.FormatInt(val, 10)); err != nil {
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
			return 0, fmt.Errorf("unsupported unary arithmetic operator: %q", expr.Op)
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
	name := b.X.(*syntax.Word).Lit()
	val := atoi(cfg.envGet(name))
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
			return 0, fmt.Errorf("division by zero")
		}
		val /= arg
	case syntax.RemAssgn:
		if arg == 0 {
			return 0, fmt.Errorf("division by zero")
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
	if err := cfg.envSet(name, strconv.FormatInt(val, 10)); err != nil {
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
			return 0, fmt.Errorf("division by zero")
		}
		return x / y, nil
	case syntax.Rem:
		if y == 0 {
			return 0, fmt.Errorf("division by zero")
		}
		return x % y, nil
	case syntax.Pow:
		if y < 0 {
			return 0, fmt.Errorf("exponent less than 0")
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
		return 0, fmt.Errorf("unsupported binary arithmetic operator: %q", op)
	}
}
