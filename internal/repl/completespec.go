package repl

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/blairham/koi-shell/internal/shell/syntax"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/builtins"
	"github.com/blairham/koi-shell/internal/complete"
	"github.com/blairham/koi-shell/internal/editor"
)

// `complete` and `compgen` (#159): programmable completion, bash's own.
//
// This is the largest single piece of the ecosystem koi inherits by
// speaking bash. bash-completion ships completions for hundreds of
// commands, and kubectl, docker, gh, terraform, aws and the rest each
// emit a `complete -F` registration from `<tool> completion bash`. All
// of it is written against three things: the `complete` builtin, the
// COMP_* variables, and COMPREPLY. Implement those and the corpus
// arrives on its own — no per-tool work, ever.
//
// The alternative is what every other new shell did: wait to be adopted
// tool by tool, and be a downgrade until that finishes.

// completionSpec is one registered completion.
type completionSpec struct {
	function string   // -F
	command  string   // -C
	words    []string // -W
	actions  []string // -A / -f -d -u -g, kept as compgen would name them
	options  []string // -o nospace, filenames, default, bashdefault
	// prefix, suffix, filter and glob are -P, -S, -X and -G. They are
	// not applied yet, but they are *kept*, because `complete -p` is
	// how a script saves and restores a spec (#406): dropping them
	// made `eval "$(complete -p cmd)"` silently strip half of it.
	prefix string
	suffix string
	filter string
	glob   string
	// letterSpelled records which actions were written as a letter
	// rather than as `-A name`. bash prints back the spelling it was
	// given — `-s` stays `-s` and `-A signal` stays `-A signal` — so
	// the spelling is part of the spec rather than a detail of parsing
	// (#533).
	letterSpelled map[string]bool
}

// completionSpecs maps command name to spec, plus the three catch-alls
// bash keeps: `complete -D` (default, when no spec matches),
// `complete -E` (empty line) and `complete -I` (the initial word).
type completionRegistry struct {
	byCommand map[string]completionSpec
	fallback  *completionSpec
	empty     *completionSpec
	// initial is `complete -I`, bash 5.1's spec for the *first* word of a
	// line. It is recorded so the option is not refused — bash accepts it
	// and refusing what bash accepts is worse than the silence #556 fixed
	// — and nothing consults it yet: the command position is answered by
	// internal/complete's PATH/builtin/function providers, which never ask
	// the registry (#609).
	initial *completionSpec
}

var completions = &completionRegistry{byCommand: map[string]completionSpec{}}

func resetCompletions() {
	completions = &completionRegistry{byCommand: map[string]completionSpec{}}
}

// completeBudget bounds one completion function. Tab is an explicit,
// synchronous request — unlike a keystroke repaint — so this is far
// more generous than the plugin budget. It exists only so that a
// runaway completion function returns the terminal rather than owning
// it: bash itself would block forever here.
const completeBudget = 2 * time.Second

func completeCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		hc := interp.HandlerCtx(ctx)
		switch args[0] {
		case "complete":
			return runCompleteBuiltin(hc.Stdout, hc.Stderr, hc.ErrLocation, args[1:]), nil
		case "compgen":
			return runCompgen(ctx, hc, args[1:]), nil
		case "compopt":
			return runCompopt(hc, args[1:]), nil
		}
		return next(ctx, args)
	}
}

// The option grammar of the complete/compgen/compopt family (#556).
//
// koi read these argument lists with a switch over whole words and a
// `default` that skipped anything else beginning with a dash. So an
// option koi does not have was *dropped*: `complete -Z x` answered 0,
// which says the registration happened, and a completion script whose
// intent koi cannot honor got no registration and no sign of it. bash
// refuses it — `complete: -Z: invalid option`, exit 2 — and a refusal is
// the whole value here, because the caller is a generated script nobody
// reads until it misbehaves.
//
// Refusing means parsing properly, and three rules had to be measured
// before anything could be refused safely. Each of them is a line bash
// *accepts*, so getting one wrong would trade a silent drop for a wrong
// rejection, which is worse.

// compOptSpec is one builtin's option vocabulary: the letters that stand
// alone, the letters that take an argument, and whether a `+letter` word
// is an option at all.
type compOptSpec struct {
	cmd   string
	flags string
	args  string
	plus  bool
}

var (
	// bash's own optstrings, letter for letter. compgen has no -p, -r,
	// -D, -E or -I (measured: each is `invalid option`), and -V is
	// compgen's alone — `complete -V` is a usage error, which is what
	// bash's complete.tests records as "doesn't work for complete".
	completeOpts = compOptSpec{cmd: "complete", flags: "abcdefgjkprsuvDEI", args: "oAGWPSXFC"}
	compgenOpts  = compOptSpec{cmd: "compgen", flags: "abcdefgjksuv", args: "oAGWPSXFCV"}
	compoptOpts  = compOptSpec{cmd: "compopt", flags: "DEI", args: "o", plus: true}
)

// compOptionNames is the `-o` vocabulary, and `compActionNames` the `-A`
// one. bash refuses a name outside either with exit 2 — `compgen: bogus:
// invalid action name` — and koi took both verbatim, so a misspelled
// action was a generator that produced nothing. Both lists are closed in
// bash 5.3 and every entry was confirmed against it by asking; ten of the
// actions are recognized here and answer nothing yet (#606), which is a
// separate honesty problem from accepting a name that does not exist.
var (
	compOptionNames = []string{
		"bashdefault", "default", "dirnames", "filenames",
		"noquote", "nosort", "nospace", "plusdirs",
	}
	compActionNames = []string{
		"alias", "arrayvar", "binding", "builtin", "command", "directory",
		"disabled", "enabled", "export", "file", "function", "group",
		"helptopic", "hostname", "job", "keyword", "running", "service",
		"setopt", "shopt", "signal", "stopped", "user", "variable",
	}
)

// compFlag is one option as it was given: the letter, whether it arrived
// as `+o` rather than `-o`, and its argument where it takes one.
type compFlag struct {
	letter byte
	plus   bool
	value  string
}

// compUsageError is an option error from this family. Every one of them
// is exit 2, and bash prints a second usage line after the message —
// that convention is #577's, so what is matched here is the status and
// the wording of the first line.
type compUsageError struct{ msg string }

func (e compUsageError) Error() string { return e.msg }

func compErrf(format string, a ...any) error {
	return compUsageError{msg: fmt.Sprintf(format, a...)}
}

// parseCompArgs splits one invocation into its options and its operands
// the way bash's internal_getopt does. The three measured rules:
//
//   - **Letters cluster.** `complete -df x` is `-d -f`, and `compgen
//     -bWfoo` is `-b -W foo` — an argument-taking letter swallows the
//     rest of its word. Reading `-df` as one unknown option would refuse
//     a line the corpus writes.
//   - **Options stop at the first operand.** `complete x -Z` registers
//     the names `x` and `-Z`, and `compgen -W abc abc -Z` completes
//     against `abc`; bash does not permute, so a dash-word after an
//     operand is an operand. Without this rule the refusal would reject
//     both.
//   - **A `+`-word is an option only for compopt.** `complete +o nosort
//     x` registers three names in bash — `+o`, `nosort` and `x` — rather
//     than clearing an option, while `compopt +Z foo` really is `+Z:
//     invalid option`. The usage line's `[-o|+o option]` belongs to
//     compopt alone.
func parseCompArgs(spec compOptSpec, args []string) ([]compFlag, []string, error) {
	var flags []compFlag
	for i := 0; i < len(args); i++ {
		word := args[i]
		plus := spec.plus && strings.HasPrefix(word, "+")
		if word == "--" {
			return flags, args[i+1:], nil
		}
		if len(word) < 2 || (!strings.HasPrefix(word, "-") && !plus) {
			// An operand — including a bare `-` or `+`. Options end here.
			return flags, args[i:], nil
		}
		for j := 1; j < len(word); j++ {
			c := word[j]
			sign := "-"
			if plus {
				sign = "+"
			}
			switch {
			case strings.IndexByte(spec.args, c) >= 0:
				value := word[j+1:]
				if value == "" {
					if i+1 >= len(args) {
						return nil, nil, compErrf("%s: %s%c: option requires an argument", spec.cmd, sign, c)
					}
					i++
					value = args[i]
				}
				flags = append(flags, compFlag{letter: c, plus: plus, value: value})
				j = len(word) // the rest of the word was the argument
			case strings.IndexByte(spec.flags, c) >= 0:
				flags = append(flags, compFlag{letter: c, plus: plus})
			default:
				return nil, nil, compErrf("%s: %s%c: invalid option", spec.cmd, sign, c)
			}
		}
	}
	return flags, nil, nil
}

// validateCompFlags applies the checks bash makes on an option's *value*,
// which fail the same way an unknown option did: a name outside the
// closed list was accepted and then quietly did nothing.
func validateCompFlags(cmd string, flags []compFlag) error {
	for _, f := range flags {
		switch f.letter {
		case 'o':
			if !slices.Contains(compOptionNames, f.value) {
				return compErrf("%s: %s: invalid option name", cmd, f.value)
			}
		case 'A':
			if !slices.Contains(compActionNames, f.value) {
				return compErrf("%s: %s: invalid action name", cmd, f.value)
			}
		case 'V':
			// A plain identifier, no subscript: bash refuses
			// `compgen -V 'arr[2]'` where `printf -v` would take it.
			if !validVarName(f.value) {
				return compErrf("%s: `%s': not a valid identifier", cmd, f.value)
			}
		}
	}
	return nil
}

// validVarName reports whether s is a shell name — the check bash's
// `compgen -V` makes, which is stricter than `printf -v`'s.
func validVarName(s string) bool {
	if s == "" || !isNameStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isNameChar(s[i]) {
			return false
		}
	}
	return true
}

// compStatus answers with an arbitrary exit status, the way printf's and
// history's handlers do: a CallHandler can only rewrite the call, and
// `true`/`false` cover 0 and 1 but not bash's 2 for a usage error.
func compStatus(status int) []string {
	switch status {
	case 0:
		return []string{"true"}
	case 1:
		return []string{"false"}
	}
	return []string{"eval", fmt.Sprintf("(exit %d)", status)}
}

// runCompopt adjusts options mid-completion. The adjustment is still
// accepted and ignored — the only one that changes what the user sees
// here is nospace, and a wrong space is a papercut while an error in the
// middle of a completion is a broken shell — but an option bash refuses
// is now refused, and what compopt does not do is written down in #612
// rather than answered with a 0.
func runCompopt(hc interp.HandlerContext, args []string) []string {
	flags, _, err := parseCompArgs(compoptOpts, args)
	if err == nil {
		err = validateCompFlags(compoptOpts.cmd, flags)
	}
	if err != nil {
		hc.Errf("%v\n", err)
		return compStatus(2)
	}
	return []string{"true"}
}

// runCompleteBuiltin parses a `complete` registration.
// errLoc is the caller's interp.HandlerContext.ErrLocation, for the
// same reason runBind takes one (#611).
func runCompleteBuiltin(out, errOut io.Writer, errLoc string, args []string) []string {
	flags, operands, err := parseCompArgs(completeOpts, args)
	if err == nil {
		err = validateCompFlags(completeOpts.cmd, flags)
	}
	if err != nil {
		fmt.Fprintf(errOut, "%s%v\n", errLoc, err)
		return compStatus(2)
	}

	var spec completionSpec
	names := slices.Clone(operands)
	remove, print := false, false
	target := &spec

	for _, f := range flags {
		switch f.letter {
		case 'F':
			target.function = f.value
		case 'C':
			target.command = f.value
		case 'W':
			target.words = append(target.words, strings.Fields(f.value)...)
		case 'A':
			target.actions = append(target.actions, f.value)
		case 'o':
			target.options = append(target.options, f.value)
		case 'f', 'd', 'c', 'b', 'u', 'g', 'j', 's', 'v', 'e', 'a', 'k':
			// The letter actions. Files, directories, commands and
			// builtins koi generates itself; users, groups, jobs and
			// signals it recognizes and does not answer yet (#606) —
			// recognized either way, so they never become command names.
			//
			// Stored under the *long* name compgen and `-A` use, so a
			// spec round-trips: kept as the bare letter, `complete -e`
			// printed back as `complete -A e` (#533).
			long := actionLongName(string(f.letter))
			target.actions = append(target.actions, long)
			target.markLetterSpelled(long)
		case 'r':
			remove = true
		case 'p':
			print = true
		case 'D':
			names = append(names, "\x00default")
		case 'E':
			names = append(names, "\x00empty")
		case 'I':
			names = append(names, "\x00initial")
		case 'P':
			target.prefix = f.value
		case 'S':
			target.suffix = f.value
		case 'X':
			target.filter = f.value
		case 'G':
			target.glob = f.value
		}
	}

	switch {
	case print && len(names) > 0:
		// `complete -p name` prints just that spec, and says so when
		// there is none — koi listed everything and answered 0.
		missing := false
		for _, n := range names {
			if _, ok := completions.byCommand[n]; !ok {
				fmt.Fprintf(errOut, "%scomplete: %s: no completion specification\n", errLoc, n)
				missing = true
				continue
			}
			printCompletions(out, n)
		}
		if missing {
			return []string{"false"}
		}
		return []string{"true"}
	case print, len(args) == 0:
		// Bare `complete` lists, as bash does. fzf pipes exactly that
		// into grep to decide whether to install its default handler,
		// so answering with a usage error there is not a cosmetic
		// difference — it is a branch taken wrongly.
		printCompletions(out)
		return []string{"true"}
	case remove:
		if len(names) == 0 {
			resetCompletions()
			return []string{"true"}
		}
		for _, n := range names {
			delete(completions.byCommand, n)
		}
		return []string{"true"}
	case len(names) == 0:
		// Options but no name to attach them to. bash's status here is 2,
		// like every other usage error in this family (#556) — koi
		// answered 1, which is a completion script's "no such spec".
		fmt.Fprintln(errOut, "complete: usage: complete [-abcdefgjksuv] [-o option] [-F function] [-W wordlist] name ...")
		return compStatus(2)
	}

	for _, n := range names {
		switch n {
		case "\x00default":
			s := spec
			completions.fallback = &s
		case "\x00empty":
			s := spec
			completions.empty = &s
		case "\x00initial":
			s := spec
			completions.initial = &s
		default:
			completions.byCommand[n] = spec
		}
	}
	return []string{"true"}
}

func printCompletions(out io.Writer, only ...string) bool {
	names := slices.Sorted(maps.Keys(completions.byCommand))
	if len(only) > 0 {
		names = nil
		for _, n := range only {
			if _, ok := completions.byCommand[n]; ok {
				names = append(names, n)
			}
		}
	}
	for _, name := range names {
		fmt.Fprintf(out, "%s %s\n", completions.byCommand[name].commandLine(), name)
	}
	return len(names) == len(only) || len(only) == 0
}

// markLetterSpelled records that an action arrived as a bare letter, so
// `complete -p` prints it back the way it was written (#533).
func (spec *completionSpec) markLetterSpelled(action string) {
	if spec.letterSpelled == nil {
		spec.letterSpelled = map[string]bool{}
	}
	spec.letterSpelled[action] = true
}

// actionLongName maps a one-letter action to the name `-A` and compgen
// use for it. The letters and the names are two spellings of one set,
// and the spec keeps the names so that printing can choose the letter.
func actionLongName(letter string) string {
	switch letter {
	case "f":
		return "file"
	case "d":
		return "directory"
	case "c":
		return "command"
	case "b":
		return "builtin"
	case "u":
		return "user"
	case "g":
		return "group"
	case "j":
		return "job"
	case "s":
		return "signal"
	case "v":
		return "variable"
	case "e":
		return "export"
	case "a":
		return "alias"
	case "k":
		return "keyword"
	}
	return letter
}

// commandLine renders a spec as the `complete` call that would
// recreate it, in bash's option order.
func (spec completionSpec) commandLine() string {
	var b strings.Builder
	b.WriteString("complete")
	// bash prints the pieces in a canonical order rather than the order
	// they were given, and sorts what it can: `-o` options
	// alphabetically, then the letter actions alphabetically, then the
	// `-A name` forms, then the value-carrying options in a fixed
	// sequence. Measured, since a listing a script diffs has to be
	// stable (#533).
	for _, o := range slices.Sorted(slices.Values(spec.options)) {
		b.WriteString(" -o " + o)
	}
	var letters, named []string
	for _, a := range spec.actions {
		if spec.letterSpelled[a] {
			letters = append(letters, actionLetter(a))
			continue
		}
		named = append(named, a)
	}
	slices.Sort(letters)
	for _, l := range letters {
		b.WriteString(" -" + l)
	}
	for _, n := range named {
		b.WriteString(" -A " + n)
	}
	if spec.glob != "" {
		b.WriteString(" -G " + singleQuote(spec.glob))
	}
	if len(spec.words) > 0 {
		b.WriteString(" -W " + singleQuote(strings.Join(spec.words, " ")))
	}
	if spec.prefix != "" {
		b.WriteString(" -P " + singleQuote(spec.prefix))
	}
	if spec.suffix != "" {
		b.WriteString(" -S " + singleQuote(spec.suffix))
	}
	if spec.filter != "" {
		b.WriteString(" -X " + singleQuote(spec.filter))
	}
	if spec.command != "" {
		b.WriteString(" -C " + singleQuote(spec.command))
	}
	if spec.function != "" {
		b.WriteString(" -F " + spec.function)
	}
	return b.String()
}

// actionLetter is actionLongName's inverse, for printing back a spec
// that was written with letters.
func actionLetter(long string) string {
	for _, l := range []string{"u", "g", "j", "s", "v", "e", "a", "k"} {
		if actionLongName(l) == long {
			return l
		}
	}
	switch long {
	case "file":
		return "f"
	case "directory":
		return "d"
	case "command":
		return "c"
	case "builtin":
		return "b"
	}
	return long
}

// runCompgen generates candidates the way bash's compgen does. It is a
// builtin in its own right because completion functions call it far
// more often than the shell does — `compgen -W "$opts" -- "$cur"` is
// the single most common line in the whole bash-completion corpus.
func runCompgen(ctx context.Context, hc interp.HandlerContext, args []string) []string {
	flags, operands, err := parseCompArgs(compgenOpts, args)
	if err == nil {
		err = validateCompFlags(compgenOpts.cmd, flags)
	}
	if err != nil {
		hc.Errf("%v\n", err)
		return compStatus(2)
	}

	// bash takes the *first* operand as the word being completed and
	// ignores the rest; koi took the last, which only ever agreed by
	// accident.
	cur := ""
	if len(operands) > 0 {
		cur = operands[0]
	}
	if len(flags) == 0 {
		// Nothing was asked for, so nothing failed: `compgen`, `compgen
		// abc` and `compgen --` all answer 0 in bash, where a generator
		// that produced nothing answers 1. koi answered 1 for both, which
		// is a script's "no candidates" for a question never asked.
		return []string{"true"}
	}

	var words, actions []string
	prefix, suffix, filter, vname := "", "", "", ""
	for _, f := range flags {
		switch f.letter {
		case 'W':
			words = append(words, compgenWords(ctx, hc, f.value)...)
		case 'A':
			actions = append(actions, f.value)
		case 'f', 'd', 'c', 'b', 'k', 'v', 'e', 'a', 'u', 'g', 'j', 's':
			actions = append(actions, actionLongName(string(f.letter)))
		case 'P':
			prefix = f.value
		case 'S':
			suffix = f.value
		case 'X':
			filter = f.value
		case 'V':
			// A repeated -V takes the last name, as bash does.
			vname = f.value
		case 'o', 'C', 'F', 'G':
			// Options, external and function generators: not here.
		}
	}

	out := append(words, actionCandidates(hc, actions, cur)...) //nolint:gocritic // a fresh slice is intended
	var matched []string
	for _, w := range out {
		if !strings.HasPrefix(w, cur) {
			continue
		}
		if filter != "" && globMatch(filter, w) {
			continue
		}
		matched = append(matched, prefix+w+suffix)
	}
	slices.Sort(matched)
	matched = slices.Compact(matched)

	if vname != "" {
		return compgenAssign(hc, vname, matched)
	}
	if len(matched) == 0 {
		return []string{"false"} // bash: nothing generated is a failure
	}
	for _, m := range matched {
		fmt.Fprintln(hc.Stdout, m)
	}
	return []string{"true"}
}

// compgenAssign is `compgen -V name`: bash 5.3's option for putting the
// candidates in an array instead of on stdout. It is the shape a
// completion function reaches for to avoid `arr=( $(compgen …) )` — a
// subshell, and with it the word splitting that turns one candidate
// containing a space into two.
//
// Every rule here was measured rather than reasoned about, and three of
// them are surprising. The array is **replaced**, not appended to. It is
// **created even when nothing was generated**, so a caller reading
// `${#arr[@]}` sees 0 rather than an unset name. And the status is
// **still 1** in that case, which is what lets a caller test either the
// status or the array and get the same answer. `-o nosort` makes no
// difference — compgen does not sort in bash at all (#613) — and nothing
// is printed.
//
// The assignment goes back through `eval` because a CallHandler cannot
// reach the variable scope itself, and the scope is the point: bash's
// `-V` inside a function with a `local` of that name writes the local
// (printf -v's route, #55).
func compgenAssign(hc interp.HandlerContext, name string, cands []string) []string {
	quoted := make([]string, 0, len(cands))
	for _, c := range cands {
		q, err := syntax.Quote(c, syntax.LangBash)
		if err != nil {
			// A candidate with a null byte or invalid UTF-8 cannot travel
			// through eval. Saying so beats assigning something subtly
			// different from what was generated.
			hc.Errf("compgen: cannot assign to %s: %v\n", name, err)
			return compStatus(1)
		}
		quoted = append(quoted, q)
	}
	assign := name + "=(" + strings.Join(quoted, " ") + ")"
	if len(cands) == 0 {
		// The array still gets created, and the status is still the
		// failure the empty listing would have been.
		return []string{"eval", assign + "; (exit 1)"}
	}
	return []string{"eval", assign}
}

// compgenWords expands a -W wordlist, which arrives as one string and
// is subject to expansion and splitting — `compgen -W "$(git remote)"`
// is ordinary in the corpus.
func compgenWords(ctx context.Context, hc interp.HandlerContext, list string) []string {
	if !strings.ContainsAny(list, "$`") {
		return strings.Fields(list)
	}
	runner := sessionRunner()
	if runner == nil {
		return strings.Fields(list)
	}
	var buf strings.Builder
	sub := runner.Subshell()
	interp.StdIO(nil, &buf, io.Discard)(sub) //nolint:errcheck // in-memory writer
	file, err := syntax.NewParser().Parse(strings.NewReader("printf '%s\\n' "+list), "compgen")
	if err != nil {
		return strings.Fields(list)
	}
	if err := sub.Run(ctx, file); err != nil {
		return strings.Fields(list)
	}
	_ = hc
	return strings.Fields(buf.String())
}

// actionCandidates produces the named classes compgen knows. cur is the
// word being completed: file and directory list the directory it points
// at, the way bash does — `compgen -f sub/` answers about sub/, not
// about the cwd.
func actionCandidates(hc interp.HandlerContext, actions []string, cur string) []string {
	var out []string
	for _, a := range actions {
		switch a {
		case "file", "directory":
			// Bare names, no trailing separator: these are candidates a
			// script feeds back into a path or compares against a name
			// from elsewhere, and a separator bash never put there makes
			// "$dir/$name" into dir//name. The line editor is the other
			// consumer and wants the slash, which is why the shaping
			// lives in complete.Files and not here.
			for _, e := range complete.Entries(cur, hc.Dir, true) {
				if a == "file" || e.IsDir {
					out = append(out, e.Value)
				}
			}
		case "command":
			runner := sessionRunner()
			path := ""
			if runner != nil {
				path = pathVar(runner)
			}
			for _, c := range complete.Commands("", path, builtins.ShellBuiltins()) {
				out = append(out, c.Value)
			}
		case "builtin", "enabled":
			// `builtin` is every builtin name; `enabled` is that list
			// minus what `enable -n` turned off, and `disabled` below is
			// the complement (#603). The split is bash's: a disabled
			// builtin is still a builtin name, so only the state
			// listings move when one is switched off.
			off := hc.DisabledBuiltins()
			for _, name := range builtins.ShellBuiltins() {
				if a == "enabled" && slices.Contains(off, name) {
					continue
				}
				out = append(out, name)
			}
		case "disabled":
			out = append(out, hc.DisabledBuiltins()...)
		case "setopt":
			out = append(out, interp.OptionNames()...)
		case "shopt":
			out = append(out, interp.ShoptNames()...)
		case "helptopic":
			// What `help` can answer about, which is this shell's set
			// rather than bash's: koi has fewer builtins and some of
			// its own (#269's rule, applied to a third listing). Both
			// of help's tables answer here — the commands and the
			// syntax topics (#557) — since a completion menu offering
			// `while` and a `help while` that works are the same
			// feature seen from two sides.
			out = append(out, helpTopicNames()...)
		case "keyword":
			// All twenty-two of bash's. coproc was the one deliberate
			// omission — this action answers what *this* shell has, and
			// listing a keyword koi dropped on the floor would have been
			// the wrong kind of parity — and it is here now that koi runs
			// it (#287).
			out = append(out, "if", "then", "else", "elif", "fi", "case", "esac",
				"for", "while", "until", "do", "done", "function", "select", "time",
				"coproc", "!", "[[", "]]", "{", "}", "in")
		case "variable", "export":
			// Runner.Vars holds the environment koi was launched with and is
			// not updated as it runs, so reading it answered with real names
			// and none of the ones the script had set (#264). Environ is the
			// live view.
			if runner := sessionRunner(); runner != nil {
				var names []string
				runner.Environ().Each(func(name string, vr expand.Variable) bool {
					if vr.IsSet() && (a == "variable" || vr.Exported) {
						names = append(names, name)
					}
					return true
				})
				slices.Sort(names)
				out = append(out, slices.Compact(names)...)
			}
		case "alias":
			// bash's own complete.tests asks for this one through `-V`
			// (`compgen -a -X 'fo*' -V vv -P 'unalias -- ' f`), which is
			// how a script builds the `unalias` line to undo its own
			// definitions. The remaining action classes that answer
			// nothing are #606.
			if runner := sessionRunner(); runner != nil {
				out = append(out, slices.Sorted(maps.Keys(runner.Aliases()))...)
			}
		case "function":
			// The other way a harness asks which functions exist, alongside
			// declare -F. Answering nothing here is indistinguishable from a
			// shell with no functions, so a snapshot generator built on
			// compgen carried none of the user's functions across.
			if runner := sessionRunner(); runner != nil {
				out = append(out, slices.Sorted(maps.Keys(runner.Funcs))...)
			}
		}
	}
	return out
}

// globMatch is compgen's -X filter: a pattern whose matches are
// *removed*.
func globMatch(pattern, s string) bool {
	pattern = strings.TrimPrefix(pattern, "!")
	ok, err := filepath.Match(pattern, s)
	return err == nil && ok
}

// bashCompletions runs a registered completion for the line, if there is
// one. ok=false means no spec applies and the core providers should
// answer instead.
func bashCompletions(runner *interp.Runner, line string, cursor int) (cands []editor.Candidate, nospace, ok bool) {
	fields := strings.Fields(line[:cursor])
	if len(fields) == 0 {
		if completions.empty == nil {
			return nil, false, false
		}
		return runCompletionSpec(runner, *completions.empty, line, cursor, "")
	}
	cmd := fields[0]
	spec, found := completions.byCommand[cmd]
	if !found {
		// The command may be a path (`/usr/bin/git`); bash keys on the
		// last element, and so does every tool that registers one.
		if base := filepath.Base(cmd); base != cmd {
			spec, found = completions.byCommand[base]
		}
	}
	if !found {
		if completions.fallback == nil {
			return nil, false, false
		}
		spec = *completions.fallback
	}
	return runCompletionSpec(runner, spec, line, cursor, cmd)
}

// runCompletionSpec sets the COMP_* variables, runs the completion, and
// reads COMPREPLY back.
//
// The variables are the contract, and all of them matter: a completion
// function reads COMP_WORDS and COMP_CWORD to find what is being
// completed, and COMP_LINE and COMP_POINT to handle the cases where
// word splitting is not enough.
func runCompletionSpec(runner *interp.Runner, spec completionSpec, line string, cursor int, cmd string) ([]editor.Candidate, bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), completeBudget)
	defer cancel()

	words, cword := compWords(line, cursor)
	cur, prev := "", ""
	if cword < len(words) {
		cur = words[cword]
	}
	if cword > 0 {
		prev = words[cword-1]
	}

	var generated []string
	switch {
	case spec.function != "":
		setCompVars(ctx, runner, line, cursor, words, cword)
		defer clearCompVars(ctx, runner)
		call := fmt.Sprintf("%s %s %s %s", spec.function,
			doubleQuoteLiteral(cmd), doubleQuoteLiteral(cur), doubleQuoteLiteral(prev))
		if err := runHookSource(ctx, runner, call); err != nil {
			// A completion function that fails has still often filled
			// COMPREPLY — bash reads it either way, and so do we.
			_ = err
		}
		generated = compreply(runner)
	case spec.command != "":
		generated = externalCompleter(ctx, spec.command, cmd, cur, prev)
	}
	generated = append(generated, matching(spec.words, cur)...)
	if len(spec.actions) > 0 {
		generated = append(generated, matching(actionCandidates(interp.HandlerContext{Dir: runner.Dir}, spec.actions, cur), cur)...)
	}
	if len(generated) == 0 {
		// An empty result is still an answer: a spec that matched and
		// produced nothing must not silently fall back to file names,
		// which is exactly the behavior people file bugs about.
		return nil, false, true
	}

	nospace := slices.Contains(spec.options, "nospace")
	out := make([]editor.Candidate, 0, len(generated))
	for _, g := range generated {
		out = append(out, editor.Candidate{Value: g, Display: g})
	}
	return out, nospace, true
}

func matching(words []string, cur string) []string {
	var out []string
	for _, w := range words {
		if strings.HasPrefix(w, cur) {
			out = append(out, w)
		}
	}
	return out
}

// externalCompleter is `complete -C prog`: the program is run with the
// command, the current word and the previous one, and prints candidates.
// aws and terraform ship this form.
func externalCompleter(ctx context.Context, prog, cmd, cur, prev string) []string {
	fields := strings.Fields(prog)
	if len(fields) == 0 {
		return nil
	}
	args := make([]string, 0, len(fields)+2)
	args = append(args, fields[1:]...)
	args = append(args, cmd, cur, prev)
	out, err := exec.CommandContext(ctx, fields[0], args...).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// compWords splits the line the way bash's COMP_WORDS does, and reports
// which word the cursor is in.
func compWords(line string, cursor int) ([]string, int) {
	if cursor > len(line) {
		cursor = len(line)
	}
	var words []string
	cword := 0
	start := -1
	for i := 0; i <= len(line); i++ {
		atEnd := i == len(line)
		isSpace := atEnd || line[i] == ' ' || line[i] == '\t'
		switch {
		case !isSpace && start < 0:
			start = i
		case isSpace && start >= 0:
			words = append(words, line[start:i])
			if start < cursor && cursor <= i {
				cword = len(words) - 1
			}
			start = -1
		}
	}
	if cursor == len(line) && (len(line) == 0 || line[len(line)-1] == ' ') {
		// The cursor sits after a space: the word being completed is a
		// new, empty one — which is how a completion function is asked
		// for "everything valid here".
		words = append(words, "")
		cword = len(words) - 1
	} else if len(words) > 0 && cword == 0 && cursor >= len(line) {
		cword = len(words) - 1
	}
	return words, cword
}

func setCompVars(ctx context.Context, runner *interp.Runner, line string, cursor int, words []string, cword int) {
	quoted := make([]string, 0, len(words))
	for _, w := range words {
		quoted = append(quoted, doubleQuoteLiteral(w))
	}
	src := strings.Join([]string{
		"COMP_LINE=" + doubleQuoteLiteral(line),
		"COMP_POINT=" + strconv.Itoa(cursor),
		"COMP_WORDS=(" + strings.Join(quoted, " ") + ")",
		"COMP_CWORD=" + strconv.Itoa(cword),
		"COMP_TYPE=9", // TAB, as bash reports for a normal completion
		"COMP_KEY=9",
		"COMPREPLY=()",
	}, "\n")
	runHookSource(ctx, runner, src) //nolint:errcheck // best effort
}

func clearCompVars(ctx context.Context, runner *interp.Runner) {
	runHookSource(ctx, runner, "unset COMP_LINE COMP_POINT COMP_WORDS COMP_CWORD COMP_TYPE COMP_KEY COMPREPLY") //nolint:errcheck // best effort
}

// compreply reads the array a completion function filled.
func compreply(runner *interp.Runner) []string {
	v, ok := runner.Vars["COMPREPLY"]
	if !ok || !v.IsSet() {
		return nil
	}
	if v.Kind == expand.Indexed {
		return slices.Clone(v.List)
	}
	if v.Str != "" {
		return []string{v.Str}
	}
	return nil
}

// completionScriptDirs are where bash-completion keeps its corpus. They
// are read on demand rather than sourced at startup: the whole point of
// the lazy loader is that a shell does not pay for eight hundred
// completions it will not use.
func completionScriptDirs() []string {
	var dirs []string
	if d := os.Getenv("BASH_COMPLETION_USER_DIR"); d != "" {
		dirs = append(dirs, filepath.Join(d, "completions"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "bash-completion", "completions"))
	}
	dirs = append(dirs,
		"/usr/share/bash-completion/completions",
		"/usr/local/share/bash-completion/completions",
		"/opt/homebrew/share/bash-completion/completions",
		"/usr/local/etc/bash_completion.d",
		"/opt/homebrew/etc/bash_completion.d",
	)
	return dirs
}

// loadCompletionFor sources the bash-completion file for a command, if
// there is one and it has not been loaded yet.
//
// This is bash-completion's own dynamic loader, reimplemented: the
// corpus is one file per command precisely so that a shell can load
// them on demand, and sourcing all of them at startup would cost more
// than every other part of koi's startup put together.
func loadCompletionFor(runner *interp.Runner, cmd string) bool {
	if cmd == "" || strings.ContainsAny(cmd, "/\\") {
		return false
	}
	if _, done := completionLoadAttempts[cmd]; done {
		return false
	}
	completionLoadAttempts[cmd] = struct{}{}
	for _, dir := range completionScriptDirs() {
		path := filepath.Join(dir, cmd)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		return sourceCompletionFile(runner, path, cmd)
	}
	return false
}

// sourceCompletionFile loads one bash-completion file. Split out so the
// budget's cancel is scoped to the one load rather than deferred inside
// the search loop.
func sourceCompletionFile(runner *interp.Runner, path, cmd string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), completeBudget)
	defer cancel()
	runHookSource(ctx, runner, ". "+singleQuote(path)) //nolint:errcheck // a broken completion file is not the shell's problem
	_, found := completions.byCommand[cmd]
	return found
}

// completionLoadAttempts remembers which commands were looked for, so a
// command with no completion file costs one stat per directory once.
var completionLoadAttempts = map[string]struct{}{}
