// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package interp

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"mvdan.cc/sh/v3/syntax"
)

// non-empty string is true, empty string is false
// pushNotDown rewrites !(a && b) as (!a) && b, which is what bash's
// precedence means. It recurses so `! a || b || c` negates only a, and
// stops at anything that is not a logical operator.
func pushNotDown(not *syntax.UnaryTest) (syntax.TestExpr, bool) {
	bin, ok := not.X.(*syntax.BinaryTest)
	if !ok || (bin.Op != syntax.AndTest && bin.Op != syntax.OrTest) {
		return nil, false
	}
	inner := &syntax.UnaryTest{Op: syntax.TsNot, X: bin.X}
	left := syntax.TestExpr(inner)
	if pushed, ok := pushNotDown(inner); ok {
		left = pushed
	}
	return &syntax.BinaryTest{Op: bin.Op, X: left, Y: bin.Y}, true
}

func (r *Runner) bashTest(ctx context.Context, expr syntax.TestExpr, classic bool) string {
	switch x := expr.(type) {
	case *syntax.Word:
		if classic {
			// In the classic "test" mode, we already expanded and
			// split the list of words, so don't redo that work.
			return r.document(x)
		}
		return r.literal(x)
	case *syntax.ParenTest:
		return r.bashTest(ctx, x.X, classic)
	case *syntax.BinaryTest:
		switch x.Op {
		case syntax.TsMatchShort, syntax.TsMatch, syntax.TsNoMatch:
			str := r.literal(x.X.(*syntax.Word))
			yw := x.Y.(*syntax.Word)
			if classic { // test, [
				lit := r.literal(yw)
				if (str == lit) == (x.Op != syntax.TsNoMatch) {
					return "1"
				}
			} else { // [[
				pattern := r.pattern(yw)
				if r.matchLocale(pattern, str) == (x.Op != syntax.TsNoMatch) {
					return "1"
				}
			}
			return ""
		}
		if r.binTest(ctx, x.Op, r.bashTest(ctx, x.X, classic), r.bashTest(ctx, x.Y, classic), classic) {
			return "1"
		}
		return ""
	case *syntax.UnaryTest:
		if x.Op == syntax.TsNot {
			// bash's `!` binds to the first term, not to the whole
			// disjunction (#402): `[[ ! x || x ]]` is true. The parser
			// gives us !(x || x), so the negation is pushed down to the
			// leftmost operand of an && / || chain — and only there,
			// since `[[ ! x = y ]]` negates the comparison and
			// `[[ ! ( x || x ) ]]` negates what the parentheses group.
			if pushed, ok := pushNotDown(x); ok {
				return r.bashTest(ctx, pushed, classic)
			}
		}
		if r.unTest(ctx, x.Op, r.bashTest(ctx, x.X, classic), classic) {
			return "1"
		}
		return ""
	}
	return ""
}

// testInt reads an operand for a numeric comparison. The two forms
// disagree and both were wrong (#401, #402): `test` and `[` want a
// plain integer and *diagnose* anything else at status 2 — koi
// answered a silent 1, which flips an `if test` without a word — while
// `[[ ]]` evaluates its operands arithmetically, so `[[ 4+3 -eq 7 ]]`
// is true and an unset name is zero.
func (r *Runner) testInt(val string, classic bool) (int64, bool) {
	if !classic {
		expr, err := syntax.NewParser().Arithmetic(strings.NewReader(val))
		if err != nil || expr == nil {
			if err != nil {
				r.errf("%s: %s: arithmetic syntax error\n", r.testName(), val)
				r.exit.code = 1
			}
			return 0, err == nil
		}
		return int64(r.arithm(expr)), true
	}
	n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
	if err != nil {
		r.errf("%s: %s: integer expected\n", r.testName(), val)
		r.exit.code = 2
		return 0, false
	}
	return n, true
}

// testName is how the diagnostic addresses itself, which is the name
// the test was *invoked* by: `test`, `[`, or `[[`.
func (r *Runner) testName() string {
	if r.testCallName == "" {
		return "test"
	}
	return r.testCallName
}

// numCompare runs one numeric comparison after reading both operands,
// which is where the diagnostic lives.
func (r *Runner) numCompare(x, y string, classic bool, cmp func(a, b int64) bool) bool {
	a, ok := r.testInt(x, classic)
	if !ok {
		return false
	}
	b, ok := r.testInt(y, classic)
	if !ok {
		return false
	}
	return cmp(a, b)
}

func (r *Runner) binTest(ctx context.Context, op syntax.BinTestOperator, x, y string, classic bool) bool {
	switch op {
	case syntax.TsReMatch:
		re, err := regexp.Compile(y)
		if err != nil {
			r.exit.code = 2
			return false
		}
		m := re.FindStringSubmatch(x)
		if m == nil {
			return false
		}
		vr := expand.Variable{
			Set:  true,
			Kind: expand.Indexed,
			List: m,
		}
		r.setVar("BASH_REMATCH", vr)
		return true
	case syntax.TsNewer, syntax.TsOlder:
		info1, err1 := r.stat(ctx, x)
		info2, err2 := r.stat(ctx, y)
		// -ot is the mirror of -nt, so swap the operands and share the logic.
		if op == syntax.TsOlder {
			info1, info2, err1, err2 = info2, info1, err2, err1
		}
		// True if the first operand exists and the second does not,
		// or if both exist and the first is newer.
		if err1 != nil {
			return false
		}
		if err2 != nil {
			return true
		}
		return info1.ModTime().After(info2.ModTime())
	case syntax.TsDevIno:
		info1, err1 := r.stat(ctx, x)
		info2, err2 := r.stat(ctx, y)
		if err1 != nil || err2 != nil {
			return false
		}
		return os.SameFile(info1, info2)
	case syntax.TsEql:
		return r.numCompare(x, y, classic, func(a, b int64) bool { return a == b })
	case syntax.TsNeq:
		return r.numCompare(x, y, classic, func(a, b int64) bool { return a != b })
	case syntax.TsLeq:
		return r.numCompare(x, y, classic, func(a, b int64) bool { return a <= b })
	case syntax.TsGeq:
		return r.numCompare(x, y, classic, func(a, b int64) bool { return a >= b })
	case syntax.TsLss:
		return r.numCompare(x, y, classic, func(a, b int64) bool { return a < b })
	case syntax.TsGtr:
		return r.numCompare(x, y, classic, func(a, b int64) bool { return a > b })
	case syntax.AndTest:
		return x != "" && y != ""
	case syntax.OrTest:
		return x != "" || y != ""
	case syntax.TsBefore:
		return x < y
	case syntax.TsAfter:
		return x > y
	default:
		// Should only happen if we forgot a case above.
		panic(fmt.Sprintf("unexpected binary test operator: %q", op))
	}
}

func (r *Runner) statMode(ctx context.Context, name string, mode os.FileMode) bool {
	info, err := r.stat(ctx, name)
	return err == nil && info.Mode()&mode != 0
}

func (r *Runner) unTest(ctx context.Context, op syntax.UnTestOperator, x string, classic bool) bool {
	switch op {
	case syntax.TsExists:
		_, err := r.stat(ctx, x)
		return err == nil
	case syntax.TsRegFile:
		info, err := r.stat(ctx, x)
		return err == nil && info.Mode().IsRegular()
	case syntax.TsDirect:
		return r.statMode(ctx, x, os.ModeDir)
	case syntax.TsCharSp:
		return r.statMode(ctx, x, os.ModeCharDevice)
	case syntax.TsBlckSp:
		info, err := r.stat(ctx, x)
		return err == nil && info.Mode()&os.ModeDevice != 0 &&
			info.Mode()&os.ModeCharDevice == 0
	case syntax.TsNmPipe:
		return r.statMode(ctx, x, os.ModeNamedPipe)
	case syntax.TsSocket:
		return r.statMode(ctx, x, os.ModeSocket)
	case syntax.TsSmbLink:
		info, err := r.lstat(ctx, x)
		return err == nil && info.Mode()&os.ModeSymlink != 0
	case syntax.TsSticky:
		return r.statMode(ctx, x, os.ModeSticky)
	case syntax.TsUIDSet:
		return r.statMode(ctx, x, os.ModeSetuid)
	case syntax.TsGIDSet:
		return r.statMode(ctx, x, os.ModeSetgid)
	case syntax.TsModif:
		info, err := r.stat(ctx, x)
		if err != nil {
			return false
		}
		return info.ModTime().After(getAtime(info))
	case syntax.TsRead:
		return r.access(ctx, x, AccessRead) == nil
	case syntax.TsWrite:
		return r.access(ctx, x, AccessWrite) == nil
	case syntax.TsExec:
		return r.access(ctx, x, AccessExec) == nil
	case syntax.TsNoEmpty:
		info, err := r.stat(ctx, x)
		return err == nil && info.Size() > 0
	case syntax.TsFdTerm:
		// -t takes a descriptor *number*, and anything else is a
		// diagnostic at status 2 in both forms — koi answered a silent
		// 1, so `test -t /dev/tty` looked like "not a terminal"
		// (#401, #402).
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			r.errf("%s: %s: integer expected\n", r.testName(), x)
			r.exit.code = 2
			return false
		}
		fd := int(n)
		var f any
		switch fd {
		case 0:
			f = r.stdin
		case 1:
			f = r.stdout
		case 2:
			f = r.stderr
		}
		if f, ok := f.(interface{ Fd() uintptr }); ok {
			// Support [os.File.Fd] methods such as the one on [*os.File].
			return term.IsTerminal(int(f.Fd()))
		}
		// TODO: allow term.IsTerminal here too if running in the
		// "single process" mode.
		return false
	case syntax.TsEmpStr:
		return x == ""
	case syntax.TsNempStr:
		return x != ""
	case syntax.TsOptSet:
		if opt, _ := r.posixOptByName(x); opt != nil {
			return *opt
		}
		return false
	case syntax.TsVarSet:
		return r.varIsSet(x)
	case syntax.TsRefVar:
		return r.lookupVar(x).Kind == expand.NameRef
	case syntax.TsNot:
		return x == ""
	case syntax.TsUsrOwn, syntax.TsGrpOwn:
		return r.unTestOwnOrGrp(ctx, op, x)
	default:
		// Should only happen if we forgot a case above.
		panic(fmt.Sprintf("unexpected unary test op: %v", op))
	}
}
