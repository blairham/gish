// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package interp

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"maps"
	mathrand "math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/shinternal"
	"mvdan.cc/sh/v3/syntax"
)

func newOverlayEnviron(parent expand.Environ, background bool) *overlayEnviron {
	oenv := &overlayEnviron{}
	if !background {
		oenv.parent = parent
	} else {
		// We could do better here if the parent is also an overlayEnviron;
		// measure with profiles or benchmarks before we choose to do so.
		for name, vr := range parent.Each {
			oenv.Set(name, vr)
		}
	}
	return oenv
}

// overlayEnviron is our main implementation of [expand.WriteEnviron].
type overlayEnviron struct {
	// parent is non-nil if [values] is an overlay over a parent environment
	// which we can safely reuse without data races, such as non-background subshells
	// or function calls.
	parent expand.Environ

	// values maps normalized variable names, per [overlayEnviron.normalize].
	values map[string]namedVariable

	// We need to know if the current scope is a function's scope, because
	// functions can modify global variables. When true, [parent] must not be nil.
	funcScope bool
}

// namedVariable records the original name of a variable for platforms
// where variable names are matched in a case-insensitive way.
type namedVariable struct {
	// TODO(v4): consider adding this field to [expand.Variable],
	// as a general way for a variable to report its original name.
	// This can be useful for GOOS=windows with case insensitive env vars,
	// as otherwise it's not possible to Environ.Get a var
	// and know what was its original name without looping over Environ.Each.
	Name string
	expand.Variable
}

func (o *overlayEnviron) normalize(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

func (o *overlayEnviron) Get(name string) expand.Variable {
	normalized := o.normalize(name)
	if vr, ok := o.values[normalized]; ok {
		return vr.Variable
	}
	if o.parent != nil {
		return o.parent.Get(name)
	}
	return expand.Variable{}
}

func (o *overlayEnviron) Set(name string, vr expand.Variable) error {
	normalized := o.normalize(name)
	prev, inOverlay := o.values[normalized]
	// Manipulation of a global var inside a function.
	if o.funcScope && !vr.Local && !prev.Local {
		// In a function, the parent environment is ours, so it's always read-write.
		return o.parent.(expand.WriteEnviron).Set(name, vr)
	}
	if !inOverlay && o.parent != nil {
		prev.Variable = o.parent.Get(name)
	}

	if o.values == nil {
		o.values = make(map[string]namedVariable)
	}
	if vr.Kind == expand.KeepValue {
		vr.Kind = prev.Kind
		vr.Str = prev.Str
		vr.List = prev.List
		vr.Indexes = prev.Indexes
		vr.Map = prev.Map
	} else if prev.ReadOnly {
		return fmt.Errorf("readonly variable")
	}
	if !vr.IsSet() { // unsetting
		if prev.Local {
			vr.Local = true
			o.values[normalized] = namedVariable{name, vr}
			return nil
		}
		delete(o.values, normalized)
	}
	// modifying the entire variable
	vr.Local = prev.Local || vr.Local
	o.values[normalized] = namedVariable{name, vr}
	return nil
}

func (o *overlayEnviron) Each(f func(name string, vr expand.Variable) bool) {
	if o.parent != nil {
		o.parent.Each(f)
	}
	for _, vr := range o.values {
		if !f(vr.Name, vr.Variable) {
			return
		}
	}
}

func execEnv(env expand.Environ) []string {
	list := make([]string, 0, 64)
	for name, vr := range env.Each {
		if !vr.IsSet() {
			// If a variable is set globally but unset in the
			// runner, we need to ensure it's not part of the final
			// list. Seems like zeroing the element is enough.
			// This is a linear search, but this scenario should be
			// rare, and the number of variables shouldn't be large.
			for i, kv := range list {
				if strings.HasPrefix(kv, name+"=") {
					list[i] = ""
				}
			}
		}
		if vr.Exported && vr.Kind == expand.String {
			list = append(list, name+"="+vr.String())
		}
	}
	return list
}

func (r *Runner) lookupVar(name string) expand.Variable {
	if name == "" {
		panic("variable name must not be empty")
	}
	var vr expand.Variable
	switch name {
	case "#":
		vr.Kind, vr.Str = expand.String, strconv.Itoa(len(r.Params))
	case "@", "*":
		vr.Kind = expand.Indexed
		if r.Params == nil {
			// r.Params may be nil but positional parameters always exist
			vr.List = []string{}
		} else {
			vr.List = r.Params
		}
	case "!":
		if n := len(r.bgProcs); n > 0 {
			vr.Kind, vr.Str = expand.String, "g"+strconv.Itoa(n)
		}
	case "?":
		vr.Kind, vr.Str = expand.String, strconv.Itoa(int(r.lastExit.code))
	case "-":
		vr.Kind, vr.Str = expand.String, r.optionFlags()
	// The call-frame views (#266). Computed on read rather than published
	// on every call and return: three arrays rebuilt per frame push would
	// be paid by every function call in every loop, to serve a reader that
	// most scripts never have.
	case shellFuncNameVar:
		if v := r.funcNameVar(); v.Set {
			return v
		}
		return expand.Variable{}
	case shellSourceVar:
		if v := r.sourceVar(); v.Set {
			return v
		}
		return expand.Variable{}
	case shellLineNoVar:
		if v := r.lineNoVar(); v.Set {
			return v
		}
		return expand.Variable{}
	case "$":
		vr.Kind, vr.Str = expand.String, strconv.Itoa(os.Getpid())
	case "PPID":
		vr.Kind, vr.Str = expand.String, strconv.Itoa(os.Getppid())
	case "RANDOM": // not for cryptographic use
		vr.Kind, vr.Str = expand.String, strconv.Itoa(mathrand.IntN(32767))
		// TODO: support setting RANDOM to seed it
	case "SRANDOM": // pseudo-random generator from the system
		var p [4]byte
		cryptorand.Read(p[:])
		n := binary.NativeEndian.Uint32(p[:])
		vr.Kind, vr.Str = expand.String, strconv.FormatUint(uint64(n), 10)
	case "DIRSTACK":
		vr.Kind, vr.List = expand.Indexed, r.dirStack
	case "0":
		vr.Kind = expand.String
		if r.filename != "" {
			vr.Str = r.filename
		} else {
			vr.Str = "gosh"
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if i := int(name[0] - '1'); i < len(r.Params) {
			vr.Kind = expand.String
			vr.Str = r.Params[i]
		}
	}
	if vr.Kind != expand.Unknown {
		vr.Set = true
		return vr
	}
	if vr := r.writeEnv.Get(name); vr.Declared() {
		return vr
	}
	return expand.Variable{}
}

// bashFlagOrder is the order bash renders `$-` in, taken from the
// shell_flags table in its flags.c: lowercase then uppercase, alphabetical
// within each, with the invocation letter appended last. Read off a real
// bash rather than from the source, because the rendering order is what
// callers see and nothing documents it — `set -aefuxC` answers `aefhuxBCc`
// there, koi's own `h` being absent for the reason shellflags.go gives.
//
// A letter absent from here is one no shell reports, so an embedder
// supplying one is dropped rather than appended somewhere arbitrary.
const bashFlagOrder = "abefhikmnprtuvxBCEHPT" + "cs"

// optionFlags renders `$-`: one letter per option currently set.
//
// This is a *probe*, which is what makes a wrong answer worse than no
// answer. The idiom it exists for is `case $- in *e*)`, used by any
// library that saves and restores options around a risky section —
// `[[ $- == *e* ]] && restore=1; set +e; …` — so a `$-` that does not
// track `set -e` does not merely fail to inform, it tells the caller
// errexit was off and gets it left off afterwards. The script then runs
// past the failure it was written to stop at, silently (#265).
//
// The letters come from two owners and are merged rather than chosen
// between. This package knows the options it implements and when they
// change; it cannot know whether the shell around it is interactive, has
// job control, or was started with -c, so those arrive through the
// environment under the same name and are unioned in. An embedder that
// supplies nothing still gets a correct answer for everything set here,
// which also keeps `set -u; echo $-` from being a fatal unbound variable.
func (r *Runner) optionFlags() string {
	var set ['z' + 1]bool
	for i, opt := range &posixOptsTable {
		// pipefail has no letter, in bash exactly as here, so it is
		// `set -o`-only and simply does not appear.
		if opt.flag != ' ' && r.opts[i] {
			set[opt.flag] = true
		}
	}
	for _, b := range []byte(r.writeEnv.Get("-").String()) {
		if int(b) < len(set) {
			set[b] = true
		}
	}
	var sb strings.Builder
	for _, b := range []byte(bashFlagOrder) {
		if set[b] {
			sb.WriteByte(b)
		}
	}
	return sb.String()
}

func (r *Runner) envGet(name string) string {
	return r.lookupVar(name).String()
}

func (r *Runner) delVar(name string) {
	if err := r.writeEnv.Set(name, expand.Variable{}); err != nil {
		r.errf("%s: %v\n", name, err)
		r.exit.code = 1
		return
	}
}

func (r *Runner) setVarString(name, value string) {
	r.setVar(name, expand.Variable{Set: true, Kind: expand.String, Str: value})
}

func (r *Runner) setVar(name string, vr expand.Variable) {
	if r.opts[optAllExport] {
		vr.Exported = true
	}
	if err := r.writeEnv.Set(name, vr); err != nil {
		r.errf("%s: %v\n", name, err)
		r.exit.code = 1
		return
	}
}

func (r *Runner) setVarWithIndex(prev expand.Variable, name string, index syntax.ArithmExpr, vr expand.Variable) {
	if vr.Kind == expand.String && index == nil {
		// When assigning a string to an array, fall back to the
		// zero value for the index.
		switch prev.Kind {
		case expand.Indexed:
			index = &syntax.Word{Parts: []syntax.WordPart{
				&syntax.Lit{Value: "0"},
			}}
		case expand.Associative:
			index = &syntax.Word{Parts: []syntax.WordPart{
				&syntax.DblQuoted{},
			}}
		}
	}
	if index == nil {
		r.setVar(name, vr)
		return
	}

	// from the syntax package, we know that value must be a string if index
	// is non-nil; nested arrays are forbidden.
	valStr := vr.Str

	var list []string
	var indexes []int
	switch prev.Kind {
	case expand.String:
		list = append(list, prev.Str)
	case expand.Indexed:
		// TODO: only clone when inside a subshell and getting a var from outside for the first time
		list = slices.Clone(prev.List)
		indexes = slices.Clone(prev.Indexes)
	case expand.Associative:
		// if the existing variable is already an AssocArray, try our
		// best to convert the key to a string
		w, ok := index.(*syntax.Word)
		if !ok {
			return
		}
		k := r.literal(w)

		// TODO: only clone when inside a subshell and getting a var from outside for the first time
		prev.Map = maps.Clone(prev.Map)
		if prev.Map == nil {
			prev.Map = make(map[string]string)
		}
		prev.Map[k] = valStr
		r.setVar(name, prev)
		return
	}
	k := r.arithm(index)
	if k < 0 {
		// Negative indices count from one past the maximum index.
		if k += shinternal.IndexedMax(list, indexes) + 1; k < 0 {
			r.errf("%s: bad array subscript\n", name)
			r.exit.code = 1
			return
		}
	}
	list, indexes = shinternal.SetIndexedElem(list, indexes, k, valStr)
	prev.Kind = expand.Indexed
	prev.List = list
	prev.Indexes = indexes
	r.setVar(name, prev)
}

// cutElemSubscript splits an array element argument like `a[3]`, as used by
// the unset builtin, into the array name and the subscript between brackets.
func cutElemSubscript(arg string) (name, sub string, ok bool) {
	i := strings.IndexByte(arg, '[')
	if i > 0 && strings.HasSuffix(arg, "]") && syntax.ValidName(arg[:i]) {
		return arg[:i], arg[i+1 : len(arg)-1], true
	}
	return "", "", false
}

// unsetElem unsets a single element of an indexed or associative array, like
// `unset 'a[3]'`. Unsetting an indexed array element may leave a hole.
func (r *Runner) unsetElem(name, sub string) {
	vr := r.lookupVar(name)
	if n, v := vr.Resolve(r.writeEnv); n != "" {
		name, vr = n, v
	}
	switch vr.Kind {
	case expand.Indexed:
		if sub == "@" || sub == "*" {
			r.delVar(name)
			return
		}
		expr, err := syntax.NewParser().Arithmetic(strings.NewReader(sub))
		if err != nil {
			r.errf("unset: %s[%s]: bad array subscript\n", name, sub)
			r.exit.code = 1
			return
		}
		if expr == nil {
			return // an empty subscript like `unset 'a[]'` is a no-op
		}
		k := r.arithm(expr)
		if k < 0 {
			// Negative indices count from one past the maximum index.
			if k += shinternal.IndexedMax(vr.List, vr.Indexes) + 1; k < 0 {
				r.errf("unset: %s[%s]: bad array subscript\n", name, sub)
				r.exit.code = 1
				return
			}
		}
		// TODO: only clone when inside a subshell and getting a var from outside for the first time
		vr.List = slices.Clone(vr.List)
		vr.Indexes = slices.Clone(vr.Indexes)
		vr.List, vr.Indexes = shinternal.DeleteIndexedElem(vr.List, vr.Indexes, k)
		r.setVar(name, vr)
	case expand.Associative:
		if sub == "@" || sub == "*" {
			r.delVar(name)
			return
		}
		// TODO: only clone when inside a subshell and getting a var from outside for the first time
		vr.Map = maps.Clone(vr.Map)
		delete(vr.Map, sub)
		r.setVar(name, vr)
	case expand.String:
		// A scalar can be unset via subscript zero.
		if sub == "0" {
			r.delVar(name)
		} else {
			r.errf("unset: %s: not an array variable\n", name)
			r.exit.code = 1
		}
	}
}

func (r *Runner) setFunc(name string, body *syntax.Stmt) {
	if r.Funcs == nil {
		r.Funcs = make(map[string]*syntax.Stmt, 4)
	}
	r.Funcs[name] = body
	// Where it was defined, for BASH_SOURCE (#266). Recorded here because
	// this is the only moment that knows: the body is a [syntax.Stmt],
	// which carries a line but not a file, and by the time it is called
	// the current file may be another one entirely.
	if r.funcSource == nil {
		r.funcSource = make(map[string]string, 4)
	}
	r.funcSource[name] = r.currentSource()
}

// currentSource is the file being executed right now: the innermost
// frame's, or the parse name at the top level.
//
// The parse name rather than mainScript, because those differ for a
// command string and bash reports the difference: `bash -c 'f(){ …; }; f'`
// gives BASH_SOURCE the shell's own $0 even though there is no `main`
// frame. mainScript answers the narrower question of whether that frame
// exists at all.
func (r *Runner) currentSource() string {
	if len(r.frames) > 0 {
		return r.frames[0].source
	}
	return r.filename
}

func stringIndex(index syntax.ArithmExpr) bool {
	w, ok := index.(*syntax.Word)
	if !ok || len(w.Parts) != 1 {
		return false
	}
	switch w.Parts[0].(type) {
	case *syntax.DblQuoted, *syntax.SglQuoted:
		return true
	}
	return false
}

// TODO: make assignVal and [setVar] consistent with the [expand.WriteEnviron] interface

// arithmStr evaluates a string as an arithmetic expression, as an assignment to
// a variable declared with "declare -i" does. An empty value and a name which
// is not set are both zero, matching bash. A value which does not parse is a
// fatal error there, so it is one here too.
func (r *Runner) arithmStr(s string) string {
	expr, err := syntax.NewParser().Arithmetic(strings.NewReader(s))
	if err != nil {
		r.errf("%s: arithmetic syntax error\n", s)
		r.exit.code = 1
		r.exit.exiting = true
		return "0"
	}
	if expr == nil {
		return "0"
	}
	return strconv.Itoa(r.arithm(expr))
}

func (r *Runner) assignVal(name string, prev expand.Variable, as *syntax.Assign, valType string) (string, expand.Variable) {
	// danglingRef records a nameref with no target — `declare -n foo` on a
	// variable that was unset. bash assigns to the nameref variable itself
	// there and *keeps* the attribute, so the value assigned becomes the
	// name it now points at; dropping to a plain string instead would
	// silently un-declare it.
	danglingRef := false
	// `declare -n name=value` sets *name*'s reference and never follows an
	// existing one: retargeting a nameref is the point of writing it
	// again. Following it instead pointed the old *target* at the new
	// name, so `declare -n r=a; declare -n r=b` left a chain r->a->b —
	// which happens to resolve to the right value and is the wrong shape,
	// visible the moment anything lists or prints the attributes (#277).
	if valType != "-n" {
		if n, v := prev.Resolve(r.writeEnv); n != "" {
			name, prev = n, v
		} else if prev.Kind == expand.NameRef {
			danglingRef = true
		}
	}
	if danglingRef {
		valType = "-n"
	}
	prev.Set = true
	if as.Value != nil {
		s := r.literal(as.Value)
		if !as.Append {
			prev.Kind = expand.String
			if valType == "-n" {
				prev.Kind = expand.NameRef
			}
			if prev.Integer {
				s = r.arithmStr(s)
			}
			prev.Str = s
			return name, prev
		}
		switch prev.Kind {
		case expand.String, expand.Unknown:
			prev.Kind = expand.String
			if prev.Integer {
				// "n+=x" on an integer variable adds rather than concatenates.
				prev.Str = r.arithmStr(prev.Str + "+(" + s + ")")
				return name, prev
			}
			prev.Str += s
		case expand.Indexed:
			// Appends to the element at index 0, creating it if unset.
			if len(prev.List) > 0 && (prev.Indexes == nil || prev.Indexes[0] == 0) {
				prev.List[0] += s
			} else {
				prev.List, prev.Indexes = shinternal.SetIndexedElem(prev.List, prev.Indexes, 0, s)
			}
		case expand.Associative:
			// TODO
		}
		return name, prev
	}
	if as.Array == nil {
		// don't return the zero value, as that's an unset variable
		prev.Kind = expand.String
		if valType == "-n" {
			prev.Kind = expand.NameRef
		}
		prev.Str = ""
		if prev.Integer {
			prev.Str = "0"
		}
		return name, prev
	}
	// Array assignment.
	elems := as.Array.Elems
	if valType == "" {
		valType = "-a" // indexed
		if len(elems) > 0 && stringIndex(elems[0].Index) {
			valType = "-A" // associative
		}
	}
	if valType == "-A" {
		amap := make(map[string]string, len(elems))
		for _, elem := range elems {
			k := r.literal(elem.Index.(*syntax.Word))
			amap[k] = r.literal(elem.Value)
		}
		if !as.Append {
			prev.Kind = expand.Associative
			prev.Map = amap
			return name, prev
		}
		// TODO
		return name, prev
	}
	// The base array which the new elements are set on; empty unless
	// we are appending to an existing value.
	var list []string
	var indexes []int
	if as.Append {
		switch prev.Kind {
		case expand.Unknown:
		case expand.String:
			list = []string{prev.Str}
		case expand.Indexed:
			// TODO: only clone when inside a subshell and getting a var from outside for the first time
			list = slices.Clone(prev.List)
			indexes = slices.Clone(prev.Indexes)
		case expand.Associative:
			// TODO
			return name, prev
		default:
			// Should only happen if we forgot a case above.
			panic(fmt.Sprintf("unexpected conversion of kind %d", prev.Kind))
		}
	}
	// Evaluate values for each array element. An explicit index like
	// [5]=x resets our index counter, which otherwise advances for every
	// value, starting after the maximum index of the base array.
	index := shinternal.IndexedMax(list, indexes) + 1
	for _, elem := range elems {
		if elem.Index != nil {
			// Index resets our index with a literal value.
			index = r.arithm(elem.Index)
			if index < 0 {
				// Negative indices count from one past the maximum index.
				if index += shinternal.IndexedMax(list, indexes) + 1; index < 0 {
					r.errf("%s: bad array subscript\n", name)
					r.exit.code = 1
					break
				}
			}
			list, indexes = shinternal.SetIndexedElem(list, indexes, index, r.literal(elem.Value))
			index++
		} else {
			// Implicit index, advancing for every word.
			for _, val := range r.fields(elem.Value) {
				list, indexes = shinternal.SetIndexedElem(list, indexes, index, val)
				index++
			}
		}
	}
	if list == nil {
		// An empty array like a=() must still expand to zero fields.
		list = []string{}
	}
	prev.Kind = expand.Indexed
	prev.List = list
	prev.Indexes = indexes
	return name, prev
}

// unsetNameRef serves `declare +n`, which detaches a nameref (#277).
//
// The order is bash's and it is the whole subtlety: any assignment is
// performed *first*, through the reference, and only then is the
// attribute removed. So with foo pointing at bar,
//
//	typeset +n foo=other
//
// leaves bar="other" and foo="bar" — foo keeps the target's *name* as
// its own value, because that is what a nameref's value has always been.
// Detaching first would have assigned "other" to foo itself and lost
// both halves.
func (r *Runner) unsetNameRef(name string, as *syntax.Assign) {
	self := r.lookupVar(name)
	if !as.Naked {
		// Assign through the reference, exactly as a plain `foo=other`
		// would while the attribute is still on.
		target, tv := r.assignVal(name, self, as, "")
		r.setVar(target, tv)
	}
	if self.Kind == expand.NameRef {
		self.Kind = expand.String
	}
	r.setVar(name, self)
}
