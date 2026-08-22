// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package expand

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/blairham/koi-shell/internal/shell/pattern"
	"github.com/blairham/koi-shell/internal/shell/shinternal"
	"github.com/blairham/koi-shell/internal/shell/syntax"
)

func nodeLit(node syntax.Node) string {
	if word, ok := node.(*syntax.Word); ok {
		return word.Lit()
	}
	return ""
}

// UnsetParameterError is returned when a parameter expansion encounters an
// unset variable and [Config.NoUnset] has been set.
type UnsetParameterError struct {
	Node    *syntax.ParamExp
	Message string
}

func (u UnsetParameterError) Error() string {
	return fmt.Sprintf("%s: %s", u.Node.Param.Value, u.Message)
}

// BadOperatorError is a `${x@…}` whose transform bash has no letter for,
// on a parameter that has a value. Unlike every other bad substitution it
// is *fatal*: bash abandons the shell rather than the input unit, exactly
// as an unbound variable under `set -u` does — measured in a script file,
// inside a function, under `source` and in a subshell, where only that
// subshell dies (#602). The shape errors are the recoverable kind and
// stay a plain error; this one needs its own type so the interpreter can
// tell them apart, since the wording is identical.
type BadOperatorError struct {
	Node *syntax.ParamExp
}

func (b BadOperatorError) Error() string {
	return fmt.Sprintf("%s: bad substitution", nodeText(b.Node))
}

// paramTransformValid reports whether text is a `${x@…}` transform bash
// has, which is the whole text between the `@` and the brace rather than
// its first letter: `${x@QQ}`, `${x@"Q"}` and `${x@$q}` are all refused
// where `${x@Q}` is not, so the operator is the source as written and
// never an expansion of it (measured).
func paramTransformValid(text string) bool {
	switch text {
	case "Q", "E", "P", "A", "K", "a", "k", "u", "U", "L":
		return true
	}
	return false
}

// paramTransformNeedsValue reports whether a `${x@…}` transform looks at
// the parameter's value at all. The four that do not are what bash asks
// about *before* the value: a parameter with no value answers the empty
// string for every other letter, valid or not, which is why `${x@nope}`
// on an unset x is not an error — while `${n@a}` on a `declare -i n` that
// was never assigned still answers `i` (both measured).
func paramTransformNeedsValue(text string) bool {
	switch text {
	case "a", "A", "k", "K":
		return false
	}
	return true
}

// badLengthOperator reports whether an expansion is one of the `${#…}`
// shapes bash reads and then refuses while expanding (#672).
//
// `${#}` is the parameter count, and an operator after that `#` is not a
// name, so bash reads the `#` as the *parameter* rather than as the
// length prefix — `${#/2/X}` really does replace in the count, and
// `${#%2}` really does remove a suffix from it. What it refuses is the
// operator with nothing after it: `${#%}`, `${#=}`, `${#+}` and `${#/}`,
// the four listed by name in bash's own source beside the rule, whose
// condition is that the operator is followed immediately by the closing
// brace. `${#:}` is the fifth and is already answered by the empty-slice
// rule; `${#-}` and `${#?}` are neither, since `-` and `?` are parameter
// names and those are the length of `$-` and `$?`.
func badLengthOperator(pe *syntax.ParamExp) bool {
	if pe.Param == nil || pe.Param.Value != "#" || pe.Length || pe.Excl ||
		pe.Width || pe.IsSet || pe.Index != nil {
		return false
	}
	if pe.Repl != nil {
		// `${#//}` has a second slash between the operator and the
		// brace, which is what makes it an ordinary replacement.
		return !pe.Repl.All && emptyWord(pe.Repl.Orig) && pe.Repl.With == nil
	}
	if pe.Exp == nil {
		return false
	}
	switch pe.Exp.Op {
	case syntax.UpperFirst, syntax.UpperAll, syntax.LowerFirst,
		syntax.LowerAll, syntax.ToggleFirst, syntax.ToggleAll:
		// A case-conversion operator is refused whatever follows it —
		// `${#^}`, `${#^^}`, `${#,a}` and `${#~a}` are all bad
		// substitutions (measured) — because bash reads its character
		// into the *name* and then finds `#^` is not a length expression.
		return true
	}
	if !emptyWord(pe.Exp.Word) {
		return false
	}
	switch pe.Exp.Op {
	case syntax.RemSmallSuffix, syntax.AssignUnset, syntax.AlternateUnset:
		// Only the one-character spellings: `${#%%}`, `${#:=}` and
		// `${#:+}` all put a character between the `#` and the brace.
		return true
	}
	return false
}

func emptyWord(w *syntax.Word) bool {
	return w == nil || len(w.Parts) == 0
}

func overridingUnset(pe *syntax.ParamExp) bool {
	if pe.Exp == nil {
		return false
	}
	switch pe.Exp.Op {
	case syntax.AlternateUnset, syntax.AlternateUnsetOrNull,
		syntax.DefaultUnset, syntax.DefaultUnsetOrNull,
		syntax.ErrorUnset, syntax.ErrorUnsetOrNull,
		syntax.AssignUnset, syntax.AssignUnsetOrNull:
		return true
	}
	return false
}

func (cfg *Config) paramExp(pe *syntax.ParamExp) (string, error) {
	oldParam := cfg.curParam
	cfg.curParam = pe
	defer func() { cfg.curParam = oldParam }()

	if pe.Bad {
		// A shape the parser could read and this language does not have
		// — a nested `${${a}}` in bash. bash reports it here rather than
		// while parsing, which ends the command and lets a script file
		// carry on (#277). Before the Param check below, since a nested
		// expansion has no name of its own.
		return "", fmt.Errorf("%s: bad substitution", nodeText(pe))
	}
	if pe.Param == nil { // e.g. zsh's ${}
		return "", fmt.Errorf("unsupported")
	}
	if badLengthOperator(pe) {
		// A `${#` followed by an operator and nothing else. bash reads
		// it, refuses it while expanding, and loses the command (#672),
		// where koi answered the parameter count as if the operator were
		// not there.
		return "", fmt.Errorf("%s: bad substitution", nodeText(pe))
	}
	name := pe.Param.Value
	index := pe.Index
	switch name {
	case "@", "*":
		index = &syntax.Word{Parts: []syntax.WordPart{
			&syntax.Lit{Value: name},
		}}
	}
	// "*" expansions like ${*}, ${arr[*]}, or ${!prefix*} join their
	// elements with the first IFS character; others use a space.
	join := func(elems []string) string {
		if nodeLit(index) == "*" || pe.Names == syntax.NamesPrefix {
			return cfg.ifsJoin(elems)
		}
		return strings.Join(elems, " ")
	}
	var vr Variable
	switch name {
	case "LINENO":
		// This is the only parameter expansion that the environment
		// interface cannot satisfy.
		line := uint64(cfg.curParam.Pos().Line()) + cfg.LineOffset
		vr = Variable{Set: true, Kind: String, Str: strconv.FormatUint(line, 10)}
	default:
		vr = cfg.Env.Get(name)
	}
	orig := vr
	// A nameref may point at an array *element*: `declare -n b="a[1]"`
	// (#389). The subscript is evaluated at each use rather than when
	// the reference was declared — `a[i]` follows i — so the expansion
	// is restated as one on the target with that index and re-run,
	// which is also how the indirection cases below reach an operator.
	if vr.Kind == NameRef && pe.Index == nil {
		if base, sub, ok := cfg.nameRefElem(vr); ok {
			if idx := subscriptWord(sub); idx != nil {
				pe2 := *pe
				pe2.Param = &syntax.Lit{Value: base}
				pe2.Index = idx
				return cfg.paramExp(&pe2)
			}
		}
	}
	if n, v := vr.Resolve(cfg.Env); n != "" {
		name, vr = n, v
	}
	if cfg.NoUnset && !vr.IsSet() && !overridingUnset(pe) {
		return "", UnsetParameterError{
			Node:    pe,
			Message: "unbound variable",
		}
	}

	var sliceOffset, sliceLen int
	if pe.Slice != nil {
		if pe.Slice.Offset == nil && pe.Slice.Length == nil {
			// `${x:}` — a slice with neither half. bash reads it and
			// reports this while expanding, which ends the command and
			// not the script (#277); the parser leaves the shape here
			// rather than refusing it, so that a whole file is not lost
			// to one line of it.
			return "", fmt.Errorf("%s: bad substitution", nodeText(pe))
		}
		var err error
		if pe.Slice.Offset != nil {
			sliceOffset, err = Arithm(cfg, pe.Slice.Offset)
			if err != nil {
				return "", err
			}
		}
		if pe.Slice.Length != nil {
			sliceLen, err = Arithm(cfg, pe.Slice.Length)
			if err != nil {
				return "", err
			}
		}
	}

	var (
		str   string
		elems []string

		indexAllElements bool // true if var has been accessed with * or @ index
		callVarInd       = true
		set              = vr.IsSet()
	)

	switch nodeLit(index) {
	case "@", "*":
		switch vr.Kind {
		case Unknown:
			elems = nil
			indexAllElements = true
		case Indexed:
			indexAllElements = true
			callVarInd = false
			elems = cfg.sliceElems(pe, vr.List, vr.Indexes, name == "@" || name == "*")
			str = join(elems)
		}
	}
	if pe.Length && index != nil && !vr.IsSet() {
		// `${#foo[1 2]}` on a name that does not exist answers 0 without
		// ever reading the subscript, where `${foo[1 2]}` reports the
		// arithmetic error. Measured against 5.3, and the one exception
		// to a subscript always being evaluated (#564) — as soon as the
		// name exists, in any kind, the subscript is read again.
		callVarInd = false
	}
	if callVarInd {
		var err error
		str, set, err = cfg.varInd(vr, index)
		if err != nil {
			return "", err
		}
	}
	if !indexAllElements {
		elems = []string{str}
	}
	if name == "@" || name == "*" {
		// $@ and $* count as unset with no positional parameters:
		// ${*-x} answers x (#360).
		set = len(elems) > 0
	}

	switch {
	case pe.Length:
		n := len(elems)
		switch nodeLit(index) {
		case "@", "*":
		default:
			// In the C locale a character is a byte, so `${#x}` of a
			// two-byte character is 2 (#470).
			if cfg.CLocale() {
				n = len(str)
			} else {
				n = utf8.RuneCountInString(str)
			}
		}
		str = strconv.Itoa(n)
	case pe.Excl:
		var strs []string
		switch {
		case pe.Names != 0:
			strs = cfg.namesByPrefix(pe.Param.Value)
		case orig.Kind == NameRef && pe.Index == nil:
			// An operator after a nameref's ${!r} applies to the target's
			// *name* in bash (${!r//v/X} rewrites the string), which this
			// does not implement; the plain form is what scripts use.
			//
			// A *subscript* is the other case entirely: ${!r[@]} asks for
			// the keys of what r points at, not for its name (#389), so
			// it falls through to the index cases below on the resolved
			// target.
			strs = append(strs, orig.Str)
		case pe.Index != nil && vr.Kind == Indexed:
			strs = vr.indexedKeys()
		case pe.Index != nil && vr.Kind == Associative:
			strs = slices.Sorted(maps.Keys(vr.Map))
		case !vr.IsSet():
			// The message names the parameter that pointed nowhere
			// (#610). A diagnostic that names no variable is a search,
			// which is the same shape #584 fixed for the location.
			return "", fmt.Errorf("%s: invalid indirect expansion", indirectName(pe))
		case !syntax.ValidName(str):
			if !syntax.ValidName(name) {
				// ${!@}, ${!*} and ${!1} with nothing to point at: the
				// special parameters expand to nothing rather than to an
				// invalid name, and bash prints empty where an ordinary
				// variable would error.
				return "", nil
			}
			// A target naming an array *element* is valid indirection —
			// `i='arr[1]'; echo ${!i}` reads that element — as is a
			// positional or special parameter, since the target is a
			// parameter name rather than a variable name (#610). Both
			// are restated as an expansion of the target and re-run, the
			// way an operator is below.
			if base, sub, ok := cutNameRefSubscript(str); ok {
				if idx := subscriptWord(sub); idx != nil {
					pe2 := *pe
					pe2.Excl = false
					pe2.Param = &syntax.Lit{Value: base}
					pe2.Index = idx
					return cfg.paramExp(&pe2)
				}
			}
			if specialParamName(str) {
				pe2 := *pe
				pe2.Excl = false
				pe2.Param = &syntax.Lit{Value: str}
				pe2.Index = nil
				return cfg.paramExp(&pe2)
			}
			// An empty or malformed name is bash's other message here,
			// and it names the *value* rather than the parameter:
			// `x='a b'; echo ${!x}` is "a b: invalid variable name".
			return "", fmt.Errorf("%s: invalid variable name", str)
		default:
			// An operator after the indirection applies to the *target*
			// (#277): ${!x//c/X} substitutes in the target's value,
			// ${!x:1:2} slices it, ${!x-def} defaults when the target is
			// unset. Re-expand with the target as the parameter and the
			// indirection consumed; without an operator the plain lookup
			// below keeps the fast path.
			if pe.Repl != nil || pe.Exp != nil || pe.Slice != nil {
				pe2 := *pe
				pe2.Excl = false
				pe2.Param = &syntax.Lit{Value: str}
				pe2.Index = nil
				return cfg.paramExp(&pe2)
			}
			vr = cfg.Env.Get(str)
			strs = append(strs, vr.String())
		}
		str = join(strs)
	case pe.Width:
		return "", fmt.Errorf("unsupported")
	case pe.IsSet:
		return "", fmt.Errorf("unsupported")
	case pe.Slice != nil:
		if callVarInd {
			// The offset and length are in characters, not bytes — and
			// in the C locale a character *is* a byte (#470), the same
			// rule ${#x} and pattern matching follow.
			hasOffset, hasLength := pe.Slice.Offset != nil, pe.Slice.Length != nil
			if cfg.CLocale() {
				str = string(sliceUnits([]byte(str), sliceOffset, sliceLen, hasOffset, hasLength))
			} else {
				str = string(sliceUnits([]rune(str), sliceOffset, sliceLen, hasOffset, hasLength))
			}
		} // else, elems are already sliced
	case pe.Repl != nil:
		elems, err := cfg.replaceElems(pe.Repl, elems)
		if err != nil {
			return "", err
		}
		str = join(elems)
	case pe.Exp != nil:
		// See [Config.paramQuoteCtx]: only the default/alternate/assign/
		// error family lets the surrounding quotes leak into how the
		// word reads its own single quotes.
		literalCtx := quoteNone
		switch pe.Exp.Op {
		case syntax.AlternateUnset, syntax.AlternateUnsetOrNull,
			syntax.DefaultUnset, syntax.DefaultUnsetOrNull,
			syntax.ErrorUnset, syntax.ErrorUnsetOrNull,
			syntax.AssignUnset, syntax.AssignUnsetOrNull:
			literalCtx = cfg.paramOuterQuote
		}
		oldCtx, oldExp := cfg.paramQuoteCtx, cfg.expWord
		cfg.paramQuoteCtx, cfg.expWord = literalCtx, literalCtx != quoteNone
		arg, err := Literal(cfg, pe.Exp.Word)
		cfg.paramQuoteCtx, cfg.expWord = oldCtx, oldExp
		if err != nil {
			return "", err
		}
		// markWord tells the caller its answer is the operator's word;
		// see [Config.wordResult]. An absent word — ${x:-} — has nothing
		// to re-expand and stays the flat empty string.
		markWord := func() {
			if pe.Exp.Word != nil {
				cfg.wordResult, cfg.wordResultPe = pe.Exp.Word, pe
				cfg.wordResultAssigned = false
			}
		}
		// markAssigned is markWord for the two operators whose answer is
		// the variable they just wrote; see [Config.wordResultAssigned].
		markAssigned := func() {
			markWord()
			cfg.wordResultAssigned = pe.Exp.Word != nil
		}
		switch op := pe.Exp.Op; op {
		case syntax.AlternateUnsetOrNull:
			if str == "" {
				break
			}
			fallthrough
		case syntax.AlternateUnset:
			if set {
				str = arg
				markWord()
			}
		case syntax.DefaultUnset:
			if set {
				break
			}
			fallthrough
		case syntax.DefaultUnsetOrNull:
			if str == "" {
				str = arg
				markWord()
			}
		case syntax.ErrorUnset:
			if set {
				break
			}
			fallthrough
		case syntax.ErrorUnsetOrNull:
			if str == "" {
				return "", UnsetParameterError{
					Node:    pe,
					Message: arg,
				}
			}
		case syntax.AssignUnset:
			if set {
				break
			}
			fallthrough
		case syntax.AssignUnsetOrNull:
			if str == "" {
				// The *assigned* value is the flat expansion; what the
				// caller's word sees is the word in its own context, as
				// bash splits ${a:=a\ b} into two fields while a itself
				// reads "a b".
				if err := cfg.assignElem(name, vr, index, arg); err != nil {
					return "", err
				}
				str = arg
				markAssigned()
			}
		case syntax.RemSmallPrefix, syntax.RemLargePrefix,
			syntax.RemSmallSuffix, syntax.RemLargeSuffix:
			str = join(cfg.removePatternElems(op, arg, elems))
		case syntax.UpperFirst, syntax.UpperAll,
			syntax.LowerFirst, syntax.LowerAll,
			syntax.ToggleFirst, syntax.ToggleAll:
			str = join(cfg.caseConvElems(op, arg, elems))
		case syntax.OtherParamOps:
			fields, terr := cfg.transformFields(pe, name, vr, elems, set)
			if terr != nil {
				return "", terr
			}
			str = join(fields)
		}
	}
	return str, nil
}

// transformFields answers a `${x@…}` as the fields bash answers with:
// one per element for the transforms that read the value, and a single
// answer for the four that describe the variable rather than its value
// (#647). The operator itself is text bash reads and judges when it runs
// rather than while parsing (#602).
//
// vr is the variable after nameref resolution and name is its name, which
// is what bash reports: `declare -ri x=5; declare -n r=x; ${r@A}` names x
// and answers with x's attributes rather than the reference's (measured).
func (cfg *Config) transformFields(pe *syntax.ParamExp, name string, vr Variable, elems []string, set bool) ([]string, error) {
	// The transform is the source between the `@` and the brace, not what
	// that source expands to.
	xform := ""
	if pe.Exp.Word != nil {
		xform = nodeText(pe.Exp.Word)
	}
	if !set && paramTransformNeedsValue(xform) {
		// No value, so bash answers before it has an opinion on the
		// letter: the empty string, and no complaint even for a letter it
		// does not have. `${x@Q}` on an unset x is empty rather than the
		// two quotes it would answer for an empty value, for the same
		// reason.
		return nil, nil
	}
	if len(elems) == 0 && paramTransformNeedsValue(xform) {
		// A list with no elements has no value for bash to read either,
		// so it never gets as far as the letter: `a=(); ${a[@]@nope}` is
		// the empty string where `a=(1 2); ${a[@]@nope}` is fatal.
		return nil, nil
	}
	if !paramTransformValid(xform) {
		return nil, BadOperatorError{Node: pe}
	}
	// `${a[@]@A}` describes the whole array, so which of the two shapes
	// answers depends on whether the expansion names a list at all.
	list := listExpansion(name, vr, pe.Index)
	positional := name == "@" || name == "*"
	switch xform {
	case "A":
		switch {
		case positional:
			// bash's pos_params_assignment: the `set --` that would
			// restate the parameters, with each one quoted reusably.
			if len(elems) == 0 {
				return nil, nil
			}
			quoted := make([]string, len(elems))
			for i, elem := range elems {
				quoted[i] = cfg.quoteReusable(elem)
			}
			return oneField("set -- " + strings.Join(quoted, " ")), nil
		case list:
			return oneField(cfg.arrayAssignment(name, vr)), nil
		case !vr.Declared():
			// bash answers nothing at all for a name it cannot find,
			// where quoting the empty string would answer `x=''`.
			return nil, nil
		}
		return oneField(cfg.scalarAssignment(name, vr, firstElem(elems), set)), nil
	case "a":
		if positional {
			// bash's string_transform answers nothing for `a` with no
			// variable behind it, which becomes an empty word per
			// element: `set -- a b; "${@@a}"` is a single space.
			return make([]string, len(elems)), nil
		}
		flags := vr.Flags()
		if list && vr.IsSet() {
			// The flags describe the variable and bash repeats them once
			// per element, which is what its list_transform does.
			out := make([]string, len(elems))
			for i := range out {
				out[i] = flags
			}
			return out, nil
		}
		// An unset array answers its flags once rather than never, which
		// is bash's special case in array_transform.
		return oneField(flags), nil
	case "K":
		if list && !positional {
			return oneField(cfg.kvPairs(vr)), nil
		}
	case "k":
		if list && !positional {
			return kvPairList(vr), nil
		}
	}
	out := make([]string, len(elems))
	for i, elem := range elems {
		out[i] = cfg.paramTransformValue(xform, elem)
	}
	return out, nil
}

// listExpansion reports whether an expansion names a whole list rather
// than one value: `$@`, `$*`, and a `[@]` or `[*]` subscript on a name
// that is an array. It is what decides between the two shapes `${x@A}`
// has, since bash builds `declare -a a=(…)` for an array and
// `name=value` for anything else.
func listExpansion(name string, vr Variable, index syntax.ArithmExpr) bool {
	switch name {
	case "@", "*":
		return true
	}
	switch nodeLit(index) {
	case "@", "*":
		switch vr.Kind {
		case Indexed, Associative:
			return true
		}
	}
	return false
}

// describesVariable reports whether an expansion carries one of the
// `${x@…}` transforms that answer about the variable rather than about its
// value, which have an answer even when there is no value to read.
func describesVariable(pe *syntax.ParamExp) bool {
	if pe.Exp == nil || pe.Exp.Op != syntax.OtherParamOps || pe.Exp.Word == nil {
		return false
	}
	return !paramTransformNeedsValue(nodeText(pe.Exp.Word))
}

// oneField is a transform's single answer as a field list. An empty
// answer is no field at all, which is bash returning NULL — the
// difference shows in `printf "<%s>"`, not in `echo`.
func oneField(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func firstElem(elems []string) string {
	if len(elems) == 0 {
		return ""
	}
	return elems[0]
}

// paramTransformValue is the half of a `${x@…}` that reads only the value
// it is given, which is what makes it the per-element half too: bash
// applies these to each element of a list, so `"${a[@]@Q}"` quotes every
// element rather than quoting them joined (#602).
func (cfg *Config) paramTransformValue(xform, str string) string {
	switch xform {
	case "Q", "K", "k":
		// `@K` and `@k` are `@Q` for anything that is not a whole array;
		// only there do they answer with key/value pairs (#647).
		return cfg.quoteReusable(str)
	case "E":
		tail := str
		var rns []rune
		for tail != "" {
			var rn rune
			rn, _, tail, _ = strconv.UnquoteChar(tail, 0)
			rns = append(rns, rn)
		}
		return string(rns)
	case "P":
		// TODO: implement prompt expansion (\u, \h, \w, etc.).
		return str
	case "U":
		return strings.ToUpper(str)
	case "u":
		rs := []rune(str)
		if len(rs) > 0 {
			rs[0] = unicode.ToUpper(rs[0])
			return string(rs)
		}
		return str
	case "L":
		return strings.ToLower(str)
	}
	return str
}

// transformElems applies a `${a[@]@…}` on the per-element path, where the
// elements have already been read out of the variable. It answers through
// the same function the flat path uses, so the four transforms that
// describe the variable agree on both (#647).
func (cfg *Config) transformElems(pe *syntax.ParamExp, elems []string) ([]string, error) {
	name := pe.Param.Value
	vr := cfg.Env.Get(name)
	if n, v := vr.Resolve(cfg.Env); n != "" {
		name, vr = n, v
	}
	set := vr.IsSet()
	if name == "@" || name == "*" {
		// $@ and $* count as unset with no positional parameters.
		set = len(elems) > 0
	}
	return cfg.transformFields(pe, name, vr, elems, set)
}

func removePattern(str, pat string, fromEnd, shortest bool) string {
	var mode pattern.Mode
	if shortest {
		mode |= pattern.Shortest
	}
	expr, err := pattern.Regexp(pat, mode)
	if err != nil {
		return str
	}
	switch {
	case fromEnd && shortest:
		// use .* to get the right-most shortest match
		expr = ".*(" + expr + ")$"
	case fromEnd:
		// simple suffix
		expr = "(" + expr + ")$"
	default:
		// simple prefix
		expr = "^(" + expr + ")"
	}
	// no need to check error as Translate returns one
	rx := regexp.MustCompile(expr)
	if loc := rx.FindStringSubmatchIndex(str); loc != nil {
		// remove the original pattern (the submatch)
		str = str[:loc[2]] + str[loc[3]:]
	}
	return str
}

// The helpers below never modify elems in place, as it may alias a
// variable's list of elements.

// perElemOps applies pattern removal, replacement, or case conversion to
// each element, leaving them unchanged for any whole-expansion operator.
func (cfg *Config) perElemOps(pe *syntax.ParamExp, elems []string) ([]string, error) {
	switch {
	case pe.Repl != nil:
		return cfg.replaceElems(pe.Repl, elems)
	case pe.Exp != nil:
		arg, err := Literal(cfg, pe.Exp.Word)
		if err != nil {
			return nil, err
		}
		switch op := pe.Exp.Op; op {
		case syntax.RemSmallPrefix, syntax.RemLargePrefix,
			syntax.RemSmallSuffix, syntax.RemLargeSuffix:
			return cfg.removePatternElems(op, arg, elems), nil
		case syntax.UpperFirst, syntax.UpperAll,
			syntax.LowerFirst, syntax.LowerAll,
			syntax.ToggleFirst, syntax.ToggleAll:
			return cfg.caseConvElems(op, arg, elems), nil
		case syntax.OtherParamOps:
			return cfg.transformElems(pe, elems)
		}
	}
	return elems, nil
}

// replaceElems applies a ${var/pattern/repl} replacement to each element.
func (cfg *Config) replaceElems(repl *syntax.Replace, elems []string) ([]string, error) {
	var (
		anchor byte
		orig   string
		err    error
	)
	if repl.All {
		// `${v//#aa/X}` has no anchor: measured, `#` and `%` are
		// ordinary characters in the double-slash form, so the whole
		// word is the pattern.
		orig, err = Pattern(cfg, repl.Orig)
	} else {
		anchor, orig, err = cfg.anchorPattern(repl.Orig)
	}
	if err != nil {
		return nil, err
	}
	if orig == "" && anchor == 0 {
		return elems, nil // nothing to replace
	}
	with, expandRep, err := cfg.replacementText(repl.With)
	if err != nil {
		return nil, err
	}
	n := 1
	if repl.All {
		n = -1
	}
	// In the C locale a character is a byte, so `?` replaces one byte
	// of a two-byte character (#470). Re-reading every side as one rune
	// per byte is what makes the rune-wise matcher behave that way, and
	// the answer is read back the same way.
	cLocale := cfg.CLocale()
	if cLocale {
		orig, with = LatinBytes(orig), LatinBytes(with)
	}
	// The replacement is expanded once, and rewritten per match only when
	// there is an `&` or an escape in it to rewrite: bash scans the
	// expanded string once and leaves it alone otherwise, backslashes
	// included, which is why `s='a\b'` in `${v/aa/$s}` keeps its
	// backslash (measured).
	replacement := func(matched string) string {
		if !expandRep {
			return with
		}
		return expandReplacement(with, matched)
	}
	out := make([]string, len(elems))
	for i, elem := range elems {
		if cLocale {
			elem = LatinBytes(elem)
		}
		if anchor != 0 {
			out[i] = replaceAnchored(orig, elem, replacement, anchor == '%')
		} else {
			locs := findAllIndex(orig, elem, n)
			sb := cfg.strBuilder()
			last := 0
			for _, loc := range locs {
				sb.WriteString(elem[last:loc[0]])
				if with != "" {
					sb.WriteString(replacement(elem[loc[0]:loc[1]]))
				}
				last = loc[1]
			}
			sb.WriteString(elem[last:])
			out[i] = sb.String()
		}
		if cLocale {
			out[i] = BytesOfLatin(out[i])
		}
	}
	return out, nil
}

// replacementText expands a `${v/pat/rep}` replacement and reports
// whether each match rewrites it.
//
// An unquoted `&` in the replacement is the text that matched — bash's
// patsub_replacement, on by default since 5.2 — while `\&`, `"&"` and
// `'&'` are a literal ampersand (#643). What separates them is whether
// the character survived expansion *unquoted*, which is #636's rule for
// the anchor arriving on the other half of the same operator, and which
// the finished replacement string cannot answer: quoting a character that
// is not special to the pattern engine leaves no trace in it.
//
// So the answer is bash's intermediate form (quote_string_for_repl): the
// expanded text with a backslash written in front of every `&` and `\`
// that was quoted in the source, which [expandReplacement] then reads.
// With the option off the plain expansion is the answer, since quote
// removal has already made those characters ordinary.
func (cfg *Config) replacementText(word *syntax.Word) (string, bool, error) {
	if cfg.NoPatSubReplacement {
		with, err := Literal(cfg, word)
		return with, false, err
	}
	if word == nil {
		return "", false, nil
	}
	field, err := cfg.wordFieldMode(word.Parts, quoteNone, escapeMark)
	if err != nil {
		return "", false, err
	}
	sb := cfg.strBuilder()
	for _, part := range field {
		if part.quote == quoteNone {
			sb.WriteString(part.val)
			continue
		}
		for i := range len(part.val) {
			if c := part.val[i]; c == '&' || c == '\\' {
				sb.WriteByte('\\')
			}
			sb.WriteByte(part.val[i])
		}
	}
	with := sb.String()
	return with, with != "" && shouldExpandReplacement(with), nil
}

// markedEscapes splits a literal at its backslashes, marking each byte a
// backslash quoted as quoted and dropping the backslash. See [escapeMark].
func markedEscapes(s string) []fieldPart {
	var parts []fieldPart
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			continue
		}
		if i > start {
			parts = append(parts, fieldPart{val: s[start:i]})
		}
		// quoteSingle rather than quoteDouble: a backslash quotes the
		// next byte whatever it is, which is the stronger of the two.
		parts = append(parts, fieldPart{quote: quoteSingle, val: s[i+1 : i+2]})
		i++
		start = i + 1
	}
	if start < len(s) {
		parts = append(parts, fieldPart{val: s[start:]})
	}
	return parts
}

// shouldExpandReplacement reports whether a replacement has anything for
// [expandReplacement] to do: bash's shouldexp_replacement, which is what
// makes a replacement with no `&` and no escape in it pass through
// untouched. A trailing lone backslash answers no, as it does there.
func shouldExpandReplacement(rep string) bool {
	for i := 0; i < len(rep); i++ {
		switch rep[i] {
		case '\\':
			if i++; i >= len(rep) {
				return false
			}
			if rep[i] == '&' || rep[i] == '\\' {
				return true
			}
		case '&':
			return true
		}
	}
	return false
}

// expandReplacement writes one match's replacement: bash's strcreplace
// with its escape-backslash flag. An unquoted `&` becomes the matched
// text — the empty string for an empty match, which is what makes
// `${v/#/P&}` a plain prepend — while `\&` and `\\` lose their backslash
// and stay literal.
func expandReplacement(rep, matched string) string {
	var sb strings.Builder
	sb.Grow(len(rep))
	for i := 0; i < len(rep); i++ {
		c := rep[i]
		if c == '&' {
			sb.WriteString(matched)
			continue
		}
		if c == '\\' && i+1 < len(rep) && (rep[i+1] == '&' || rep[i+1] == '\\') {
			i++
			c = rep[i]
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

// replaceAnchored replaces the single match of pat that starts at the
// beginning of str, or — with atEnd — the one that finishes at its end,
// which is what `${v/#pat/rep}` and `${v/%pat/rep}` ask for (#636).
// A pattern that does not match there leaves the value alone, and an
// empty pattern matches the empty string at that edge, so `${v/#/P}` is
// a prepend and `${v/%/S}` an append — bash's answer, and a useful test
// of whether the anchor is read at all.
//
// The match asked for is leftmost-*longest*, which is bash's rule. Every
// pattern koi translates today produces the same answer either way,
// because a glob's `*` becomes a greedy quantifier and there is no
// alternation to choose between — `@(a|abc)` is not translated at all
// yet, in the anchored form or the plain one — so Longest is set to ask
// the engine for bash's semantics rather than to rely on that staying
// true. There is no submatch to read back, since the whole match is the
// span being replaced, which is what makes Longest usable at all.
//
// The replacement is a function of the matched text rather than a string
// because an unquoted `&` in it is that text (#643) — and the anchored
// forms are exactly where bash's own comment says the rule matters, since
// an empty pattern makes them sed's `^` and `$`.
func replaceAnchored(pat, str string, replacement func(string) string, atEnd bool) string {
	expr, err := pattern.Regexp(pat, 0)
	if err != nil {
		return str
	}
	if atEnd {
		expr = "(?:" + expr + ")$"
	} else {
		expr = "^(?:" + expr + ")"
	}
	// no need to check the error as Regexp returns one
	rx := regexp.MustCompile(expr)
	rx.Longest()
	loc := rx.FindStringIndex(str)
	if loc == nil {
		return str
	}
	return str[:loc[0]] + replacement(str[loc[0]:loc[1]]) + str[loc[1]:]
}

// removePatternElems applies a pattern removal operator to each element.
func (cfg *Config) removePatternElems(op syntax.ParExpOperator, arg string, elems []string) []string {
	suffix := op == syntax.RemSmallSuffix || op == syntax.RemLargeSuffix
	small := op == syntax.RemSmallPrefix || op == syntax.RemSmallSuffix
	// Byte-wise in the C locale, like every other pattern here (#470).
	if cfg.CLocale() {
		arg = LatinBytes(arg)
	}
	out := make([]string, len(elems))
	for i, elem := range elems {
		if !cfg.CLocale() {
			out[i] = removePattern(elem, arg, suffix, small)
			continue
		}
		out[i] = BytesOfLatin(removePattern(LatinBytes(elem), arg, suffix, small))
	}
	return out
}

// caseConvElems applies a case conversion operator to each element.
func (cfg *Config) caseConvElems(op syntax.ParExpOperator, arg string, elems []string) []string {
	caseFunc := unicode.ToLower
	switch op {
	case syntax.UpperFirst, syntax.UpperAll:
		caseFunc = unicode.ToUpper
	case syntax.ToggleFirst, syntax.ToggleAll:
		// bash's `${x~}` and `${x~~}` swap the case rather than forcing
		// one, which is why this is a third function and not a flag.
		caseFunc = toggleCase
	}
	all := op == syntax.UpperAll || op == syntax.LowerAll || op == syntax.ToggleAll

	// empty string means '?'; nothing to do there
	expr, err := pattern.Regexp(arg, 0)
	if err != nil {
		return elems
	}
	rx := regexp.MustCompile(expr)

	// The C locale has no case beyond ASCII — bash leaves every other
	// byte alone — and its characters are bytes, so both the matching
	// and the mapping are per byte there (#470).
	cLocale := cfg.CLocale()
	if cLocale {
		caseFunc = asciiOnly(caseFunc)
	}
	out := make([]string, len(elems))
	for i, elem := range elems {
		rs := []rune(elem)
		if cLocale {
			rs = []rune(LatinBytes(elem))
		}
		for ri, r := range rs {
			if rx.MatchString(string(r)) {
				rs[ri] = caseFunc(r)
			}
			if !all {
				break // only the first character is considered
			}
		}
		out[i] = string(rs)
		if cLocale {
			out[i] = BytesOfLatin(out[i])
		}
	}
	return out
}

// SubscriptKey is a subscript as an associative array's key: the text
// between the brackets, expanded as one word, with the quotes removed.
// That is the whole of bash's rule and none of it is arithmetic — `m[a-b]`
// keys on `a-b`, `m[-1]` on `-1`, `m[x[1]]` on `x[1]`, and `m[ a - b ]` on
// ` a - b ` with the spaces kept, where the same text in an indexed array's
// subscript is a subtraction, a count from the end, and a reference to
// another array's element (#626).
//
// One word, so no field splitting and no globbing: `sp='a b'; m[$sp]=1`
// keys on `a b` and `m[zq*]=1` on `zq*` even where `zq1` exists. The
// expansion still happens, which is why this cannot be the raw source —
// `m[$a-$b]` keys on what the two expand to.
//
// A subscript that is not a word reaches this only from a tree built by
// hand — the parser keeps every subscript as one (#626) — and it is still
// text, so it is rendered rather than crashed on: this used to be
// `idx.(*syntax.Word)`, and the interface conversion it panicked with was
// the loudest failure koi had.
func SubscriptKey(cfg *Config, idx syntax.ArithmExpr) (string, error) {
	switch idx := idx.(type) {
	case nil:
		// `m[  ]=v`: brackets holding nothing but whitespace, which is
		// zero to an arithmetic reader and the empty key here.
		return "", nil
	case *syntax.Word:
		if idx == nil {
			return "", nil
		}
		return Literal(cfg, idx)
	}
	return nodeText(idx), nil
}

// varInd expands an indexed variable expansion like ${a[i]}, also reporting
// whether the resulting element is set, which may be false for missing array
// elements such as the holes in a sparse array.
func (cfg *Config) varInd(vr Variable, idx syntax.ArithmExpr) (string, bool, error) {
	if idx == nil {
		switch vr.Kind {
		case Indexed:
			// A bare $a is the element at index zero, which may be unset.
			str, ok := vr.indexedVal(0)
			return str, ok, nil
		case Associative:
			str, ok := vr.Map["0"]
			return str, ok, nil
		}
		return vr.String(), vr.IsSet(), nil
	}
	switch vr.Kind {
	case String:
		// ${a[@]} and ${a[*]} on a scalar are the scalar, never an
		// arithmetic index — @ would be a syntax error there (#366).
		switch nodeLit(idx) {
		case "*", "@":
			return vr.Str, vr.IsSet(), nil
		}
		n, err := Arithm(cfg, idx)
		if err != nil {
			return "", false, err
		}
		if n == 0 {
			return vr.Str, vr.IsSet(), nil
		}
	case Indexed:
		switch nodeLit(idx) {
		case "*", "@":
			return strings.Join(vr.List, " "), vr.IsSet(), nil
		}
		i, err := Arithm(cfg, idx)
		if err != nil {
			return "", false, err
		}
		if i < 0 {
			// Negative indices count from one past the maximum index.
			if i += shinternal.IndexedMax(vr.List, vr.Indexes) + 1; i < 0 {
				return "", false, fmt.Errorf("negative array index")
			}
		}
		if str, ok := vr.indexedVal(i); ok {
			return str, true, nil
		}
	case Unknown:
		// A name that does not exist is read like an indexed array, so
		// its subscript is still evaluated — and a subscript that is not
		// arithmetic is bash's error rather than an empty string:
		// `echo ${foo[1 2]}` reports the arithmetic error and abandons
		// the line where koi answered nothing at all (#564). The value is
		// discarded; only the diagnostic is the point.
		switch nodeLit(idx) {
		case "@", "*":
		default:
			if _, err := Arithm(cfg, idx); err != nil {
				return "", false, err
			}
		}
	case Associative:
		switch lit := nodeLit(idx); lit {
		case "@", "*":
			strs := slices.Sorted(maps.Values(vr.Map))
			if lit == "*" {
				return cfg.ifsJoin(strs), vr.IsSet(), nil
			}
			return strings.Join(strs, " "), vr.IsSet(), nil
		}
		key, err := SubscriptKey(cfg, idx)
		if err != nil {
			return "", false, err
		}
		str, ok := vr.Map[key]
		return str, ok, nil
	}
	return "", false, nil
}

// assignElem assigns a variable via an expansion like ${a=val} or
// ${a[i]=val}, setting a single element when the variable is an array or is
// indexed, like Bash. ${a[i]=val} on a whole scalar converts it to an array.
func (cfg *Config) assignElem(name string, vr Variable, idx syntax.ArithmExpr, val string) error {
	wenv, ok := cfg.Env.(WriteEnviron)
	if !ok {
		return fmt.Errorf("environment is read-only")
	}
	arrayWise := false
	switch nodeLit(idx) {
	case "@", "*":
		// ${a[@]=val} assigns the element at index zero.
		arrayWise = true
		idx = nil
	}
	if idx == nil && !arrayWise && vr.Kind != Indexed && vr.Kind != Associative {
		// A plain scalar assignment like ${x=val}.
		return wenv.Set(name, Variable{Set: true, Kind: String, Str: val})
	}
	switch vr.Kind {
	case Associative:
		key := "0"
		if idx != nil {
			var err error
			if key, err = SubscriptKey(cfg, idx); err != nil {
				return err
			}
		}
		vr.Map = maps.Clone(vr.Map)
		if vr.Map == nil {
			vr.Map = make(map[string]string, 1)
		}
		vr.Map[key] = val
	default: // assign a single indexed element
		i := 0
		if idx != nil {
			var err error
			if i, err = Arithm(cfg, idx); err != nil {
				return err
			}
			if i < 0 {
				// Negative indices count from one past the maximum index.
				if i += shinternal.IndexedMax(vr.List, vr.Indexes) + 1; i < 0 {
					return fmt.Errorf("negative array index")
				}
			}
		}
		list, indexes := slices.Clone(vr.List), slices.Clone(vr.Indexes)
		if vr.Kind == String {
			list, indexes = []string{vr.Str}, nil
		}
		list, indexes = shinternal.SetIndexedElem(list, indexes, i, val)
		vr.Kind, vr.Str, vr.List, vr.Indexes = Indexed, "", list, indexes
	}
	vr.Set = true
	return wenv.Set(name, vr)
}

func (cfg *Config) namesByPrefix(prefix string) []string {
	var names []string
	for name := range cfg.Env.Each {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// indirectName renders the parameter an indirect expansion pointed
// through, the way bash names it in `foo: invalid indirect expansion` and
// `foo[2]: invalid indirect expansion` — the name as written, with its
// subscript when it has one, rather than the whole `${!foo}` (#610).
func indirectName(pe *syntax.ParamExp) string {
	name := pe.Param.Value
	if pe.Index != nil {
		name += "[" + nodeText(pe.Index) + "]"
	}
	return name
}

// subscriptWord reads a nameref or indirection target's subscript text the
// way the parser reads one in place: as a whole word, whose meaning the
// array decides when the reference is used (#626). Reading it as arithmetic
// instead resolved `declare -n r=m[a-b]` to a subtraction, so a reference to
// an associative element wrote and read the wrong key. Nil means the text is
// not a subscript at all, which is what makes `x='a b'` a malformed name
// rather than an element reference.
func subscriptWord(sub string) syntax.ArithmExpr {
	idx, err := syntax.NewParser().Subscript(strings.NewReader(sub))
	if err != nil {
		return nil
	}
	return idx
}

// specialParamName reports whether a name is a positional or special
// parameter rather than a variable name — `1`, `@`, `*`, `#` and the
// rest. An indirection may point at one (`a=1; echo ${!a}` is `$1`), so
// these are not the malformed names they look like to [syntax.ValidName].
func specialParamName(name string) bool {
	if name == "" {
		return false
	}
	switch name {
	case "@", "*", "#", "?", "-", "$", "!", "_":
		return true
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// nameRefElem walks a chain of references for a link that names an array
// *element* rather than a variable, which is the one target
// [Variable.Resolve] cannot express — it answers with a name, and
// `a[1]` is not one. A reference to a reference to an element is
// ordinary in bash's own suite, where a helper takes a variable's name
// as an argument and points its own `typeset -n` at it (#610).
func (cfg *Config) nameRefElem(vr Variable) (base, sub string, ok bool) {
	for range maxNameRefDepth {
		if vr.Kind != NameRef || vr.Str == "" {
			return "", "", false
		}
		if base, sub, ok := cutNameRefSubscript(vr.Str); ok {
			return base, sub, true
		}
		vr = cfg.Env.Get(vr.Str)
	}
	return "", "", false
}

// cutNameRefSubscript splits a nameref target that names an array
// element, like `a[1]`, into the array's name and the subscript text.
func cutNameRefSubscript(target string) (name, sub string, ok bool) {
	i := strings.IndexByte(target, '[')
	if i > 0 && strings.HasSuffix(target, "]") && syntax.ValidName(target[:i]) {
		return target[:i], target[i+1 : len(target)-1], true
	}
	return "", "", false
}
