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

	"mvdan.cc/sh/v3/syntax"

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
}

// completionSpecs maps command name to spec, plus the two catch-alls
// bash keeps: `complete -D` (default, when no spec matches) and
// `complete -E` (empty line).
type completionRegistry struct {
	byCommand map[string]completionSpec
	fallback  *completionSpec
	empty     *completionSpec
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
			return runCompleteBuiltin(hc.Stdout, hc.Stderr, args[1:]), nil
		case "compgen":
			return runCompgen(ctx, hc, args[1:]), nil
		case "compopt":
			// Adjusting options mid-completion; accepted and ignored,
			// since the only one that changes what the user sees here is
			// nospace, and a wrong space is a papercut while an error in
			// the middle of a completion is a broken shell.
			return []string{"true"}, nil
		}
		return next(ctx, args)
	}
}

// runCompleteBuiltin parses a `complete` registration.
func runCompleteBuiltin(out, errOut io.Writer, args []string) []string {
	var spec completionSpec
	var names []string
	remove, print := false, false
	target := &spec

	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "-F":
			target.function = next()
		case "-C":
			target.command = next()
		case "-W":
			target.words = append(target.words, strings.Fields(next())...)
		case "-A":
			target.actions = append(target.actions, next())
		case "-o":
			target.options = append(target.options, next())
		case "-f":
			target.actions = append(target.actions, "file")
		case "-d":
			target.actions = append(target.actions, "directory")
		case "-c":
			target.actions = append(target.actions, "command")
		case "-b":
			target.actions = append(target.actions, "builtin")
		case "-u", "-g", "-j", "-s", "-v", "-e", "-a", "-k":
			// Users, groups, jobs, signals, variables, exports, aliases,
			// keywords: recognized so they do not become command names,
			// and left to compgen, which knows how to produce them.
			target.actions = append(target.actions, strings.TrimPrefix(a, "-"))
		case "-r":
			remove = true
		case "-p":
			print = true
		case "-D":
			names = append(names, "\x00default")
		case "-E":
			names = append(names, "\x00empty")
		case "-P", "-S", "-X", "-G":
			next() // prefix/suffix/filter/glob: parsed, not yet applied
		default:
			if strings.HasPrefix(a, "-") {
				continue // an option we do not know is not a command name
			}
			names = append(names, a)
		}
	}

	switch {
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
		fmt.Fprintln(errOut, "complete: usage: complete [-abcdefgjksuv] [-o option] [-F function] [-W wordlist] name ...")
		return []string{"false"}
	}

	for _, n := range names {
		switch n {
		case "\x00default":
			s := spec
			completions.fallback = &s
		case "\x00empty":
			s := spec
			completions.empty = &s
		default:
			completions.byCommand[n] = spec
		}
	}
	return []string{"true"}
}

func printCompletions(out io.Writer) {
	for _, name := range slices.Sorted(maps.Keys(completions.byCommand)) {
		spec := completions.byCommand[name]
		var b strings.Builder
		b.WriteString("complete")
		for _, o := range spec.options {
			b.WriteString(" -o " + o)
		}
		if spec.function != "" {
			b.WriteString(" -F " + spec.function)
		}
		if spec.command != "" {
			b.WriteString(" -C " + spec.command)
		}
		if len(spec.words) > 0 {
			b.WriteString(" -W " + singleQuote(strings.Join(spec.words, " ")))
		}
		fmt.Fprintf(out, "%s %s\n", b.String(), name)
	}
}

// runCompgen generates candidates the way bash's compgen does. It is a
// builtin in its own right because completion functions call it far
// more often than the shell does — `compgen -W "$opts" -- "$cur"` is
// the single most common line in the whole bash-completion corpus.
func runCompgen(ctx context.Context, hc interp.HandlerContext, args []string) []string {
	var words []string
	var actions []string
	prefix, suffix, filter := "", "", ""
	cur := ""
	sawSeparator := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "--":
			sawSeparator = true
		case "-W":
			words = append(words, compgenWords(ctx, hc, next())...)
		case "-A":
			actions = append(actions, next())
		case "-f":
			actions = append(actions, "file")
		case "-d":
			actions = append(actions, "directory")
		case "-c":
			actions = append(actions, "command")
		case "-b":
			actions = append(actions, "builtin")
		case "-k":
			actions = append(actions, "keyword")
		case "-v":
			actions = append(actions, "variable")
		case "-e":
			actions = append(actions, "export")
		case "-P":
			prefix = next()
		case "-S":
			suffix = next()
		case "-X":
			filter = next()
		case "-o", "-C", "-F", "-G":
			next() // options, external and function generators: not here
		default:
			if strings.HasPrefix(a, "-") && !sawSeparator {
				continue
			}
			cur = a
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
	if len(matched) == 0 {
		return []string{"false"} // bash: nothing generated is a failure
	}
	slices.Sort(matched)
	for _, m := range slices.Compact(matched) {
		fmt.Fprintln(hc.Stdout, m)
	}
	return []string{"true"}
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
		case "builtin":
			out = append(out, builtins.ShellBuiltins()...)
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
