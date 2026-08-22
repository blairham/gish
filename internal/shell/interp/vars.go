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
	"time"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/shinternal"
	"github.com/blairham/koi-shell/internal/shell/syntax"
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
	// Manipulation of a global var inside a function. A Global write
	// (`declare -g`, #379) descends regardless of any local shadowing
	// the name — that shadow is exactly what it exists to bypass.
	if o.funcScope && (vr.Global || (!vr.Local && !prev.Local)) {
		// In a function, the parent environment is ours, so it's always read-write.
		return o.parent.(expand.WriteEnviron).Set(name, vr)
	}
	vr.Global = false // a write-time signal, never stored
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
	if r.unsetDynamic[name] {
		// A computed variable a script has unset is an ordinary name
		// for the rest of the shell: empty until something assigns it,
		// and an ordinary variable when something does (#547).
		if vr := r.writeEnv.Get(name); vr.Declared() {
			return vr
		}
		return expand.Variable{}
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
		vr.Kind, vr.Str = expand.String, strconv.Itoa(r.randomValue())
	case "SECONDS":
		// The dynamic variables a script times itself with, and they
		// were simply absent — an empty string in arithmetic is zero,
		// so a loop measuring elapsed time never advanced (#408).
		vr.Kind, vr.Str = expand.String, strconv.Itoa(int(time.Since(r.startTime).Seconds())+r.secondsBase)
	case "EPOCHSECONDS":
		vr.Kind, vr.Str = expand.String, strconv.FormatInt(time.Now().Unix(), 10)
	case "EPOCHREALTIME":
		now := time.Now()
		vr.Kind, vr.Str = expand.String, fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000)
	case "BASH_ARGV0":
		vr.Kind, vr.Str = expand.String, r.lookupVar("0").Str
		vr.Set = true
	case "BASHPID":
		// $$ keeps the shell's pid through a subshell while BASHPID
		// reports the subshell's own. koi has no separate process for
		// a subshell, so the identity a script is really asking about
		// is "am I in a different execution context?" — answered with
		// the shell's pid at the top level and a distinct number per
		// subshell.
		vr.Kind, vr.Str = expand.String, strconv.Itoa(r.bashPID())
	case "GROUPS":
		vr.Kind, vr.Set = expand.Indexed, true
		vr.List = r.groupsList()
	case "SRANDOM": // pseudo-random generator from the system
		var p [4]byte
		cryptorand.Read(p[:])
		n := binary.NativeEndian.Uint32(p[:])
		vr.Kind, vr.Str = expand.String, strconv.FormatUint(uint64(n), 10)
	case "SHELLOPTS":
		// The "is this option on?" probe every portable script writes,
		// and it was simply absent — under `set -u` an unbound-variable
		// error (#396). Rendered on read from the live table, like $-,
		// so it cannot go stale.
		vr.Kind, vr.Str = expand.String, r.shellOptsList()
		vr.ReadOnly = true
	case "BASHOPTS":
		vr.Kind, vr.Str = expand.String, r.bashOptsList()
		vr.ReadOnly = true
	case "DIRSTACK":
		vr.Kind, vr.List = expand.Indexed, r.dirStack
	case "0":
		vr.Kind = expand.String
		if r.argv0 != "" {
			// BASH_ARGV0 is $0's writable view: assigning it renames
			// the shell for everything that reads $0 (#408).
			vr.Str = r.argv0
			return vr
		}
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
	default:
		// ${10} and beyond (#362): any all-digit name is a positional
		// parameter. Only the braced form reaches here — the parser
		// reads $10 as $1 followed by 0, as bash does.
		if n, err := strconv.Atoi(name); err == nil && n >= 10 {
			if n <= len(r.Params) {
				vr.Kind = expand.String
				vr.Str = r.Params[n-1]
			}
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
		r.preRedirErrf("%s: %v\n", name, err)
		r.exit.code = 1
		return
	}
	if name == "GLOBIGNORE" {
		// Unsetting GLOBIGNORE turns the dotglob option off — even when
		// dotglob was set by hand, and even when GLOBIGNORE was never
		// set. Measured against bash 5.3 (#375).
		r.opts[optDotGlob] = false
		r.updateExpandOpts()
	}
}

func (r *Runner) setVarString(name, value string) {
	r.setVar(name, expand.Variable{Set: true, Kind: expand.String, Str: value})
}

// dynamicVars are the variables the shell answers from its own state
// rather than from the variable table. A script can unset one, which
// ends its specialness for the rest of the shell (#547); the two that
// take a *write* are handled in [Runner.setDynamic].
//
// The readonly ones (SHELLOPTS, BASHOPTS) are absent because unset
// refuses them before reaching here, which is bash's answer too.
var dynamicVars = map[string]bool{
	"RANDOM": true, "SRANDOM": true, "SECONDS": true,
	"EPOCHSECONDS": true, "EPOCHREALTIME": true, "BASHPID": true,
	"GROUPS": true, "DIRSTACK": true, "FUNCNAME": true,
	"BASH_SOURCE": true, "BASH_LINENO": true,
}

// LINENO is absent from that list on purpose: it is answered in
// `expand`, which is the one parameter the environment interface cannot
// satisfy, so the shell's record of what a script unset never reaches
// the code that computes it. `unset LINENO` therefore still reports a
// line, where bash reports nothing. Listing it here would record the
// unset and change nothing, which is worse than the gap.

// dynamicListing are the computed variables bash's own variable table
// holds an entry for, so `declare` lists them and `declare -p NAME`
// prints them — where a listing built only from the variable table saw
// none of the shell's own arrays (#616). The value is the shape the
// variable takes when the shell has nothing to report for it.
//
// Membership is measured rather than derived from [dynamicVars], because
// bash's table does not hold every computed variable: RANDOM, SRANDOM,
// SECONDS, EPOCHSECONDS, EPOCHREALTIME, BASHPID and LINENO appear in no
// listing there, so adding them here would be a divergence of its own.
// BASH_ARGC and BASH_ARGV are absent for the opposite reason — koi does
// not have them at all yet (#637).
//
// The two empty shapes differ, and that too is measured: FUNCNAME
// outside a function prints with no value at all (`declare -a
// FUNCNAME`), while BASH_SOURCE and BASH_LINENO in a `-c` string print
// as an empty array (`declare -a BASH_SOURCE=()`).
var dynamicListing = map[string]expand.Variable{
	shellFuncNameVar: {Kind: expand.Indexed},
	shellSourceVar:   {Kind: expand.Indexed, Set: true, List: []string{}},
	shellLineNoVar:   {Kind: expand.Indexed, Set: true, List: []string{}},
	"DIRSTACK":       {Kind: expand.Indexed, Set: true, List: []string{}},
	"GROUPS":         {Kind: expand.Indexed, Set: true, List: []string{}},
	"SHELLOPTS":      {Kind: expand.String, Set: true, ReadOnly: true},
	"BASHOPTS":       {Kind: expand.String, Set: true, ReadOnly: true},
}

// dynamicListingVar answers a computed variable as a *listing* sees it,
// or an undeclared variable for a name no listing shows.
//
// A listing asks a different question from an expansion, and FUNCNAME is
// where the two answers part: bash's table holds it whether or not a
// function is running, so `declare -p FUNCNAME` prints `declare -a
// FUNCNAME` at a script's top level while `${FUNCNAME+set}` is empty and
// `[[ -v FUNCNAME ]]` is false. That distinction is load-bearing — every
// `${FUNCNAME[1]:-…}` helper depends on it — so it lives here rather
// than in [Runner.lookupVar], which keeps answering "unset".
func (r *Runner) dynamicListingVar(name string) expand.Variable {
	empty, ok := dynamicListing[name]
	if !ok || r.unsetDynamic[name] {
		// A computed variable a script has unset is an ordinary name for
		// the rest of the shell (#547), and bash stops listing it too —
		// so whatever the variable table holds for it is the whole
		// answer, and this reader has nothing to add.
		return expand.Variable{}
	}
	if vr := r.lookupVar(name); vr.Declared() {
		return vr
	}
	return empty
}

// unsetDynamicVar records that a computed variable has been unset. It is
// one-way: bash does not restore the specialness when the name is
// assigned again, so `unset RANDOM; RANDOM=5` answers 5 forever after.
func (r *Runner) unsetDynamicVar(name string) {
	if !dynamicVars[name] {
		return
	}
	if r.unsetDynamic == nil {
		r.unsetDynamic = make(map[string]bool)
	}
	r.unsetDynamic[name] = true
}

// randomValue answers $RANDOM and advances the sequence.
//
// The generator belongs to this runner and is seeded from the system
// until a script assigns RANDOM, so a subshell draws its own numbers
// without moving this one along: `x=$(echo $RANDOM)` in a loop leaves
// the parent's sequence where it was, which is what bash gets by
// reseeding a forked child (#547).
func (r *Runner) randomValue() int {
	if r.random == nil {
		r.random = mathrand.New(mathrand.NewPCG(mathrand.Uint64(), mathrand.Uint64()))
	}
	// bash's range is inclusive of 32767, and koi's was one short of it.
	return r.random.IntN(32768)
}

// setDynamic handles a write to a computed variable, and reports whether
// it took the assignment. Only two accept one: RANDOM seeds the
// generator and SECONDS moves the origin it counts from. bash ignores a
// write to the rest — LINENO, EPOCHSECONDS, BASHPID and friends keep
// answering what they answered — so they are stored and shadowed, which
// is what koi already did.
func (r *Runner) setDynamic(name string, vr expand.Variable) bool {
	if r.unsetDynamic[name] {
		return false
	}
	switch name {
	case "RANDOM":
		// The value is arithmetic, so `RANDOM=1+1` and `RANDOM=2` seed
		// alike; the sequence after it is repeatable, which is the
		// whole reason the idiom exists (#547).
		n, _ := strconv.Atoi(r.arithmStr(vr.Str))
		r.random = mathrand.New(mathrand.NewPCG(uint64(n), randomSeedStream))
		return true
	case "SECONDS":
		// Not arithmetic, measured: bash answers 0 for `SECONDS=1+1`
		// and -5 for `SECONDS=-5`.
		r.startTime, r.secondsBase = time.Now(), wholeInt(vr.Str)
		return true
	}
	return false
}

// randomSeedStream is the second half of the seed, which selects a
// stream rather than a position: PCG wants both, and a script supplies
// one number. Any constant does, so long as it never changes — the
// contract is that a seed repeats *in koi*, not that it matches bash's
// digits (#120).
const randomSeedStream = 0x9e3779b97f4a7c15

// wholeInt reads a string that is entirely an integer, and answers zero
// for anything else. Measured rather than assumed, and it is why SECONDS
// is not the arithmetic RANDOM is: bash answers 0 for `SECONDS=1+1` and
// for `SECONDS=abc`, and -5 for `SECONDS=-5`.
func wholeInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func (r *Runner) setVar(name string, vr expand.Variable) {
	if r.opts[optAllExport] {
		vr.Exported = true
	}
	if r.setDynamic(name, vr) {
		return
	}
	if vr.CaseMod != 0 {
		// -u/-l/-c transform on *every* assignment (#385), which is
		// why this sits at the one point every path reaches rather
		// than in the assignment code: a plain x=v, an append, and an
		// element write all land here. The transforms are idempotent,
		// so re-storing an unchanged variable is harmless.
		switch vr.Kind {
		case expand.Indexed:
			vr.List = slices.Clone(vr.List)
			for i, v := range vr.List {
				vr.List[i] = vr.ApplyCaseMod(v)
			}
		case expand.Associative:
			vr.Map = maps.Clone(vr.Map)
			for k, v := range vr.Map {
				vr.Map[k] = vr.ApplyCaseMod(v)
			}
		case expand.String:
			vr.Str = vr.ApplyCaseMod(vr.Str)
		}
	}
	if err := r.writeEnv.Set(name, vr); err != nil {
		// Not preRedirErrf: reaching here means a *builtin* refused the
		// write (export, declare, read), and a builtin's diagnostic
		// goes to its own stderr. The plain-assignment path reports its
		// own before the redirections (#469).
		r.errf("%s: %v\n", name, err)
		r.exit.code = 1
		return
	}
	if name == "BASH_ARGV0" {
		// Writing BASH_ARGV0 sets $0, which is the point of it.
		r.argv0 = vr.Str
	}
	if name == "GROUPS" {
		// bash discards a write to GROUPS silently rather than
		// refusing it: the array is what the kernel says (#408), and a
		// refusal would make an assignment that bash ignores fatal.
		return
	}
	if name == "OPTIND" {
		// Assigning OPTIND restarts the scan, including the position
		// *within* a clustered word — which is what makes a recursive
		// getopts work (#403). koi compared only the argument index, so
		// `typeset OPTIND=1` in a function that was mid-cluster changed
		// nothing and the recursion never terminated.
		n, err := strconv.Atoi(vr.Str)
		if err != nil || n < 1 {
			n = 1
		}
		r.optState = getopts{argidx: n - 1}
	}
	if name == "POSIXLY_CORRECT" {
		// Assigning it turns POSIX mode on, whatever the value — the
		// variable and the option are one state (#395).
		r.opts[optPosix] = true
		r.updateExpandOpts()
	}
	switch name {
	case "PATH", "SHELL", "ENV", "BASH_ENV":
		if r.opts[optRestricted] {
			// A restricted shell cannot change where commands come
			// from, which bash spells as the variable being read-only
			// (#398).
			r.errf("%s: readonly variable\n", name)
			r.exit.code = 1
			// Fatal for a command string and not for a script file,
			// which is bash's split and only visible by running both:
			// `bash -c 'set -r; PATH=/x; echo after'` prints nothing
			// after the refusal, while the same three lines in a file
			// carry on.
			if r.mainScript == "" {
				r.exit.exiting = true
			}
			return
		}
	}
	if hook := r.varHooks[name]; hook != nil {
		// The shell around the interpreter asked to hear about this
		// name: assigning it is an action rather than just a value
		// (#491).
		hook(name, vr.Str)
	}
	if name == "GLOBIGNORE" && vr.Kind == expand.String && vr.Str != "" {
		// Assigning a non-null GLOBIGNORE turns the dotglob option on.
		// bash mutates the real option — shopt reports it on and
		// shopt -u turns it back off — where a null assignment changes
		// nothing and unset turns it off. Measured against 5.3 (#375).
		r.opts[optDotGlob] = true
		r.updateExpandOpts()
	}
}

func (r *Runner) setVarWithIndex(prev expand.Variable, name string, index syntax.ArithmExpr, vr expand.Variable) {
	if name == "BASH_ARGV0" {
		// Writing BASH_ARGV0 sets $0, which is the point of it.
		r.argv0 = vr.Str
	}
	if name == "GROUPS" {
		return // discarded, as in bash (#408)
	}
	if vr.Kind == expand.String && index == nil {
		// When assigning a string to an array, fall back to the
		// zero value for the index.
		switch prev.Kind {
		case expand.Indexed:
			index = &syntax.Word{Parts: []syntax.WordPart{
				&syntax.Lit{Value: "0"},
			}}
		case expand.Associative:
			// bash lands a scalar assignment to an associative array
			// under the key "0", not the empty key: m=x on a declared
			// -A answers ([0]="x"). Measured against 5.3 (#378).
			index = &syntax.Word{Parts: []syntax.WordPart{
				&syntax.Lit{Value: "0"},
			}}
		}
	}
	if index == nil {
		r.setVar(name, vr)
		return
	}

	// The element paths below write through prev, which for a declared
	// but unset array still carries Set=false — and any attributes the
	// caller just applied live on vr, not prev. Merge both, so setting
	// an element makes the variable set (`declare -a c; c=4` must print
	// as an array, #378) and keeps freshly applied attributes
	// (`export a=5` on an array keeps -x).
	prev.Set = true
	prev.Local = vr.Local
	prev.Exported = vr.Exported
	prev.ReadOnly = vr.ReadOnly
	prev.Integer = vr.Integer
	prev.Global = vr.Global

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
		// The key is the subscript's text, whatever that text also reads
		// as: `m[a-b]=1` keys on `a-b`, where taking the arithmetic
		// reading of it dropped the assignment without a word (#626).
		k, err := expand.SubscriptKey(r.ecfg, index)
		if err != nil {
			r.expandErr(err)
			return
		}
		// A nil index is `m[  ]=v`, a subscript of nothing but
		// whitespace: an empty arithmetic expression for an indexed
		// array, and here the empty key. bash keeps the spaces
		// themselves as the key, which is what the word carries now.

		// TODO: only clone when inside a subshell and getting a var from outside for the first time
		prev.Map = maps.Clone(prev.Map)
		if prev.Map == nil {
			prev.Map = make(map[string]string)
		}
		prev.Map[k] = valStr
		r.setVar(name, prev)
		return
	}
	// `*` and `@` are the whole-array subscripts, so they are a *key* for
	// an associative array — handled above — and never an index. bash
	// answers `b[*]: bad array subscript` rather than an arithmetic
	// error, which is what koi's evaluator would otherwise say about a
	// word it cannot read as a number (#582).
	if w, ok := index.(*syntax.Word); ok {
		switch r.literal(w) {
		case "*", "@":
			r.expandErr(fmt.Errorf("%s[%s]: bad array subscript", name, subscriptText(index)))
			return
		}
	}
	// Not r.arithm: a subscript that will not read as arithmetic is an
	// assignment that does not happen, where r.arithm reports and answers
	// zero (#564). `declare -a a=(p q); a[hello world]=1` leaves bash's
	// array untouched, and answering zero wrote the value over element 0
	// while printing the error — the worst of both.
	k, err := expand.Arithm(r.ecfg, index)
	if err != nil {
		r.expandErr(err)
		return
	}
	if k < 0 {
		// Negative indices count from one past the maximum index.
		if k += shinternal.IndexedMax(list, indexes) + 1; k < 0 {
			// Named as written, and it ends the input unit rather than
			// only the command: bash abandons the rest of the line
			// (#582, #469).
			r.expandErr(fmt.Errorf("%s[%s]: bad array subscript", name, subscriptText(index)))
			return
		}
	}
	list, indexes = shinternal.SetIndexedElem(list, indexes, k, valStr)
	prev.Kind = expand.Indexed
	prev.List = list
	prev.Indexes = indexes
	r.setVar(name, prev)
}

// subscriptText renders a subscript the way it was written, so a
// diagnostic can name it the way bash's does: `c[-2]`, `b[*]`, `d[7]`.
// The empty string covers both the blank subscript and a shape the
// printer cannot render, which is what `name[]` needs anyway.
func subscriptText(index syntax.ArithmExpr) string {
	if index == nil {
		return ""
	}
	// The canonical function printer, not syntax.Printer.Print: the
	// latter takes only whole nodes it knows how to lay out and answers
	// "unsupported node type" for an arithmetic *expression*, so `c[-2]`
	// would have printed as `c[]` — a diagnostic naming the wrong
	// subscript. funcPrinter.arithm exists for exactly this (#386) and
	// renders the same text the parser recorded.
	p := &funcPrinter{wp: syntax.NewPrinter(syntax.SingleLine(true))}
	p.arithm(index)
	return p.sb.String()
}

// subscriptError is the assignment shapes bash rejects when the
// assignment *runs* rather than while parsing (#582), or nil.
//
// Reporting is the caller's, because the two callers differ in what the
// error costs: a plain assignment abandons the rest of the input unit
// while `declare` answers 1 and carries on — the same split #308
// measured for a readonly variable.
func subscriptError(name string, as *syntax.Assign) error {
	switch {
	case as.BadIndex:
		// `b[]=x`: brackets with nothing between them at all.
		return fmt.Errorf("%s[]: bad array subscript", name)
	case as.Index != nil && as.Array != nil:
		// `d[7]=(a b)`: zsh assigns a list to a member, bash does not.
		return fmt.Errorf("%s[%s]: cannot assign list to array member",
			name, subscriptText(as.Index))
	}
	return nil
}

// elemSubscriptError is a compound assignment element's subscript
// verdict, or nil: `[]=v` and a negative index out of range are both
// "bad array subscript", and `[*]=v` — a whole-array subscript where a
// number belongs — is bash's "cannot assign to non-numeric index"
// (#582). All three name the element as written, which is what bash
// prints and what tells a reader which element of many was wrong.
func elemSubscriptError(r *Runner, elem *syntax.ArrayElem) error {
	text := func() string {
		return "[" + subscriptText(elem.Index) + "]=" + elemValueText(elem.Value)
	}
	if elem.BadIndex {
		return fmt.Errorf("%s: bad array subscript", text())
	}
	w, ok := elem.Index.(*syntax.Word)
	if !ok {
		return nil
	}
	switch r.literal(w) {
	case "*", "@":
		return fmt.Errorf("%s: cannot assign to non-numeric index", text())
	}
	return nil
}

// elemValueText renders an element's value the way it was written, for
// the diagnostic above.
func elemValueText(val *syntax.Word) string {
	if val == nil {
		return ""
	}
	p := &funcPrinter{wp: syntax.NewPrinter(syntax.SingleLine(true))}
	p.word(val)
	return p.sb.String()
}

// subscriptRefused reports a plain assignment's bad subscript and says
// whether the assignment must be abandoned.
//
// It goes through expandErr rather than errf because the message needs
// the location every runtime diagnostic carries (#584) *and* the
// input-unit abandonment bash performs here (#469), and that classifier
// is where the two are decided together.
func (r *Runner) subscriptRefused(name string, as *syntax.Assign) bool {
	err := subscriptError(name, as)
	if err == nil {
		return false
	}
	r.expandErr(err)
	return true
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

// localInScope reports whether name is already a local of the function
// scope currently being run — which is what makes a second `local x`
// in the same function keep its value where the first one dropped the
// outer variable's (#381).
func (r *Runner) localInScope(name string) bool {
	o, ok := r.writeEnv.(*overlayEnviron)
	if !ok || !o.funcScope {
		return false
	}
	_, ok = o.values[o.normalize(name)]
	return ok
}

// varIsSet answers test's -v the way bash does (#378): a subscripted
// name tests that element — with @ or * meaning "any element" — and a
// bare array name tests element 0 (key "0" for an associative array),
// not whether the array has elements at all: A[a]=1 leaves [ -v A ]
// false. A scalar is element 0 of itself, so -v s[0] and -v s[@] answer
// whether s is set. Measured against 5.3.
func (r *Runner) varIsSet(x string) bool {
	name, sub, hasSub := x, "", false
	if n, s, ok := cutElemSubscript(x); ok {
		name, sub, hasSub = n, s, true
	}
	vr := r.lookupVar(name)
	if n, v := vr.Resolve(r.writeEnv); n != "" {
		vr = v
	}
	subIndex := func() (int, bool) {
		expr, err := syntax.NewParser().Arithmetic(strings.NewReader(sub))
		if err != nil || expr == nil {
			return 0, false
		}
		return r.arithm(expr), true
	}
	switch vr.Kind {
	case expand.Indexed:
		if hasSub && (sub == "@" || sub == "*") {
			return len(vr.List) > 0
		}
		k := 0
		if hasSub {
			var ok bool
			if k, ok = subIndex(); !ok {
				return false
			}
			if k < 0 {
				if k += shinternal.IndexedMax(vr.List, vr.Indexes) + 1; k < 0 {
					r.errf("%s: bad array subscript\n", name)
					return false
				}
			}
		}
		if vr.Indexes != nil {
			return slices.Contains(vr.Indexes, k)
		}
		return k >= 0 && k < len(vr.List)
	case expand.Associative:
		// @ and * are ordinary keys here, not "any element": bash
		// answers [ -v B[@] ] false on a populated associative array
		// without an "@" key — the post-5.1 literal-key rule.
		key := "0"
		if hasSub {
			key = sub
		}
		_, ok := vr.Map[key]
		return ok
	default:
		if !hasSub || sub == "@" || sub == "*" {
			return vr.IsSet()
		}
		k, ok := subIndex()
		return ok && k == 0 && vr.IsSet()
	}
}

// unsetElem unsets a single element of an indexed or associative array, like
// `unset 'a[3]'`. Unsetting an indexed array element may leave a hole.
//
// viaRef says the element was named by a nameref rather than written out,
// which changes one verdict: `unset ref` where ref points at `x[2]` and x
// is a scalar is silent in bash, where the same subscript written out is
// "not an array variable" at status 1 (#610). It reports whether the
// caller should stay at status 0.
func (r *Runner) unsetElem(name, sub string, viaRef bool) bool {
	vr := r.lookupVar(name)
	if n, v := vr.Resolve(r.writeEnv); n != "" {
		name, vr = n, v
	}
	switch vr.Kind {
	case expand.Indexed:
		if sub == "@" || sub == "*" {
			r.delVar(name)
			return true
		}
		expr, err := syntax.NewParser().Arithmetic(strings.NewReader(sub))
		if err != nil {
			r.errf("unset: %s[%s]: bad array subscript\n", name, sub)
			return false
		}
		if expr == nil {
			return true // an empty subscript like `unset 'a[]'` is a no-op
		}
		k := r.arithm(expr)
		if k < 0 {
			// Negative indices count from one past the maximum index.
			if k += shinternal.IndexedMax(vr.List, vr.Indexes) + 1; k < 0 {
				r.errf("unset: %s[%s]: bad array subscript\n", name, sub)
				return false
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
			return true
		}
		// TODO: only clone when inside a subshell and getting a var from outside for the first time
		vr.Map = maps.Clone(vr.Map)
		delete(vr.Map, sub)
		r.setVar(name, vr)
	case expand.String:
		// A scalar can be unset via subscript zero.
		switch {
		case sub == "0":
			r.delVar(name)
		case viaRef:
			// Silent in bash, and only through a reference (#610).
		default:
			r.errf("unset: %s: not an array variable\n", name)
			return false
		}
	}
	return true
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
// is not set are both zero, matching bash.
//
// A value which does not parse ends a command string and not a script
// file (#529). The comment here used to say it was fatal "there, so it
// is one here too", which is true of `-c` and measured wrong of a file:
// bash reports it, sets status 1, and runs the next line.
func (r *Runner) arithmStr(s string) string {
	expr, err := syntax.NewParser().Arithmetic(strings.NewReader(s))
	if err != nil {
		r.errf("%s: arithmetic syntax error\n", s)
		r.exit.code = 1
		r.exit.exiting = r.mainScript == ""
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
		s := r.literalAssign(as.Value)
		if !as.Append {
			prev.Kind = expand.String
			if valType == "-n" {
				// A reference's value is a *name*, so the integer
				// attribute has nothing to evaluate and bash drops it
				// (#610): `typeset -i x=1; typeset -n x=y` leaves
				// `declare -n x="y"`, where evaluating the target as
				// arithmetic pointed x at whatever y happened to hold.
				prev.Kind = expand.NameRef
				prev.Integer = false
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
		if prev.Kind == expand.Associative {
			// name=(...) and name+=(...) on an existing associative
			// array stay associative; bare words pair as key/value.
			valType = "-A"
		} else if len(elems) > 0 && stringIndex(elems[0].Index) {
			valType = "-A"
		}
	}
	if valType == "-A" {
		var amap map[string]string
		if as.Append && prev.Kind == expand.Associative {
			amap = maps.Clone(prev.Map)
		}
		if amap == nil {
			amap = make(map[string]string, len(elems))
		}
		if len(elems) > 0 && elems[0].Index == nil {
			// bash 5.1+: when the first element has no [key], the words
			// pair up as alternating key/value, an odd word out keying
			// the empty string. A later [k]=v element is not special in
			// this mode — it reads back as the literal word it was.
			var words []string
			for _, elem := range elems {
				if w, ok := elem.Index.(*syntax.Word); ok {
					words = append(words, "["+r.literal(w)+"]="+r.literal(elem.Value))
					continue
				}
				words = append(words, r.literal(elem.Value))
			}
			for i := 0; i < len(words); i += 2 {
				if words[i] == "" {
					r.errf("'': bad array subscript\n")
					continue
				}
				val := ""
				if i+1 < len(words) {
					val = words[i+1]
				}
				amap[words[i]] = val
			}
		} else {
			for _, elem := range elems {
				w, ok := elem.Index.(*syntax.Word)
				if !ok {
					if elem.Index == nil {
						// A bare word after a subscripted element is an
						// assignment error. It ends a command string and
						// not a script file (#529): bash reports it,
						// sets status 1, and runs the next line.
						r.errf("%s: %s: must use subscript when assigning associative array\n",
							name, r.literal(elem.Value))
						r.exit.code = 1
						r.exit.exiting = r.mainScript == ""
						return name, prev
					}
					r.errf("%s: bad array subscript\n", name)
					continue
				}
				amap[r.literal(w)] = r.literal(elem.Value)
			}
		}
		prev.Kind = expand.Associative
		prev.Map = amap
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
	// What a rejected element leaves behind, below.
	baseList, baseIndexes := list, indexes
	// Under declare -i, each element value is an arithmetic expression
	// (#368): typeset -i x; x=([0]=7+11) stores 18, exactly as a scalar
	// assignment under the attribute would.
	elemVal := func(val string) string {
		if prev.Integer {
			return r.arithmStr(val)
		}
		return val
	}
	for _, elem := range elems {
		// An element's subscript is read when the assignment runs, and a
		// bad one costs the *whole* compound assignment: bash reports it
		// and leaves an empty array behind, measured — `d=([]=a [1]=b)`
		// answers `declare -a d=()` (#582). The diagnostic names the
		// element as written rather than the variable.
		if err := elemSubscriptError(r, elem); err != nil {
			// Through expandErr: bash abandons the rest of the *line*
			// for these, exactly as for a bad subscript on the left of
			// the `=` — measured, `echo pre; d=([]=y); echo same` never
			// prints `same`.
			r.expandErr(err)
			// What survives is what the variable already held: an
			// append keeps its base (`d=(x); d+=([]=y)` answers
			// `[0]="x"`) and a plain assignment is left empty, since
			// its base is nothing. Every element of this assignment is
			// discarded, not only the bad one — measured.
			list, indexes = baseList, baseIndexes
			break
		}
		if elem.Index != nil {
			// Index resets our index with a literal value. Not r.arithm:
			// a subscript that will not read as arithmetic costs the
			// whole compound assignment like the verdicts above, where
			// r.arithm would report and carry on with zero (#564).
			var err error
			if index, err = expand.Arithm(r.ecfg, elem.Index); err != nil {
				r.expandErr(err)
				list, indexes = baseList, baseIndexes
				break
			}
			if index < 0 {
				// Negative indices count from one past the maximum index.
				if index += shinternal.IndexedMax(list, indexes) + 1; index < 0 {
					// Named as the element was written, like the
					// verdicts above it (#582), and abandoning the line
					// the same way.
					r.expandErr(fmt.Errorf("[%s]=%s: bad array subscript",
						subscriptText(elem.Index), elemValueText(elem.Value)))
					list, indexes = baseList, baseIndexes
					break
				}
			}
			list, indexes = shinternal.SetIndexedElem(list, indexes, index, elemVal(r.literal(elem.Value)))
			index++
		} else {
			// Implicit index, advancing for every word.
			for _, val := range r.fields(elem.Value) {
				list, indexes = shinternal.SetIndexedElem(list, indexes, index, elemVal(val))
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

// setLoopVar assigns a for or select loop's variable, following a
// nameref the way an ordinary assignment does (#389): with `declare -n
// ref` in scope, `for ref in one two` sets *one* and *two* rather than
// overwriting the reference cell with the literal names.
func (r *Runner) setLoopVar(name, value string) bool {
	prev := r.lookupVar(name)
	if prev.Kind != expand.NameRef {
		r.setVarString(name, value)
		return true
	}
	// A nameref loop variable is *re-targeted* rather than assigned
	// through: `declare -n ref; for ref in one two` walks ref over the
	// two variables, so ${!ref} names each and $ref reads its value.
	// Each item is therefore a name, and one that is not an identifier
	// is bash's error — measured, where koi wrote the literal through
	// the reference and corrupted the target (#389).
	if !syntax.ValidName(value) {
		// bash reports the first bad item and abandons the loop rather
		// than reporting each one.
		r.errf("`%s': not a valid identifier\n", value)
		r.exit.code = 1
		return false
	}
	prev.Set = true
	prev.Str = value
	r.setVar(name, prev)
	return true
}
