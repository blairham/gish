package repl

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/builtins"
)

// The help builtin (#196): bash's most-typed discovery command, and until
// now the worst kind of absent — interp.IsBuiltin recognizes the name, so
// it shadowed nothing useful and answered "unsupported builtin" while
// `type help` called it a shell builtin.
//
// It lives on the CallHandler seam rather than in nativeOverrides because
// the rewrite case needs to dispatch back into the handler chain:
// `help config` becomes `config help`, so every koi command's usage
// screen stays single-sourced. doctor and explain take no subcommand and
// get topics below instead.

// helpRewrites are the koi commands whose full usage lives behind their
// own `<name> help` screen. A name here must be in callHandlerCommands
// and must actually answer `help` — helpcmd_test.go holds both.
var helpRewrites = []string{
	"blocks", "clip", "config", "migrate", "p10k", "pick", "plugin",
	"prompt", "sandbox", "sessions", "tool", "trust", "z", "zi",
}

// helpTopic is one bash-`help`-style entry: a synopsis and a one-liner.
type helpTopic struct {
	use  string
	desc string
}

// helpNotes holds the longer explanation a couple of topics need, keyed
// by the listed name and printed under the one-liner.
//
// It is a second map rather than a third field on helpTopic because
// almost ninety entries are positional literals and none of them wants
// one. It exists because a usage line is *data* and an explanation is
// not (#611): `fc` used to print eleven lines of prose about koi's
// history positions in place of bash's one-line usage, and the prose has
// to land somewhere. `help <name>` is that somewhere — the surface a
// person asks when they want to be explained to, and the one nothing
// matches against.
var helpNotes = map[string][]string{
	"fc":     fcNotes,
	"enable": enableNotes,
}

// enableNotes explain the one pair of options koi can never implement,
// which the usage line still lists because refusing them by name is the
// honest answer and `invalid option` would read as koi not knowing the
// flag (#603).
var enableNotes = []string{
	"-f and -d are dynamic loading, and koi has none: -f loads a builtin",
	"from a shared object built against bash's own internals, so there is",
	"nothing koi could open. Both refuse in bash's own words — -f with",
	"\"dynamic loading not available\", -d with \"not dynamically loaded\" for",
	"a real builtin and \"not a shell builtin\" for anything else.",
	"",
	"A disabled builtin bypasses koi's replacements too, not just the",
	"interpreter's own: `enable -n printf` reaches the printf on PATH.",
}

func init() {
	// The session-scoped builtins' notes live beside their topics rather
	// than in one literal, since they are one group with one reason for
	// existing (#618).
	maps.Copy(helpNotes, sessionBuiltinNotes)
}

// helpTopics covers every implemented shell builtin, every koi-native
// builtin, and the koi commands with no help subcommand of their own.
// helpcmd_test.go fails when a builtin ships without an entry here.
var helpTopics = map[string]helpTopic{
	// Shell builtins — the interpreter's implemented set.
	":":         {": [arguments]", "no effect: expand the arguments and return success"},
	".":         {". file [arguments]", "run a file in the current shell (the POSIX spelling of source)"},
	"[":         {"[ expression ]", "evaluate a conditional expression — the bracket spelling of test"},
	"caller":    {"caller [n]", "report the line and function n frames up the call stack"},
	"alias":     {"alias [name[=value] ...]", "define aliases, or print the defined ones"},
	"break":     {"break [n]", "exit a for, while, or until loop (n levels out)"},
	"builtin":   {"builtin name [arguments]", "run a shell builtin even when a function shadows its name"},
	"cd":        {"cd [dir]", "change the working directory (default $HOME); cd - returns to $OLDPWD"},
	"command":   {"command name [arguments]", "run a command bypassing shell functions and aliases"},
	"compgen":   {"compgen -A action [word]", "generate the completions an action would offer"},
	"continue":  {"continue [n]", "start the next iteration of the enclosing loop"},
	"declare":   {"declare [-aAfgilnrtux] [name[=value] ...]", "declare variables and give them attributes (also spelled typeset)"},
	"dirs":      {"dirs", "print the pushd/popd directory stack"},
	"echo":      {"echo [-neE] [argument ...]", "write the arguments to standard output"},
	"eval":      {"eval [argument ...]", "join the arguments and run them as shell input"},
	"exec":      {"exec command [arguments]", "replace the shell with the command"},
	"exit":      {"exit [n]", "exit the shell with status n (default: the last command's)"},
	"export":    {"export name[=value] ...", "mark variables for export to child processes"},
	"false":     {"false", "return failure"},
	"getopts":   {"getopts optstring name [args]", "parse options from the positional parameters"},
	"hash":      {"hash [name ...]", "remember or report command locations"},
	"local":     {"local name[=value] ...", "declare variables local to the enclosing function"},
	"let":       {"let expression ...", "evaluate arithmetic expressions"},
	"mapfile":   {"mapfile [-t] [array]", "read standard-input lines into an array"},
	"popd":      {"popd", "pop the directory stack and change to the new top"},
	"printf":    {"printf format [argument ...]", "format and print the arguments (%s %d %f %q ...)"},
	"pushd":     {"pushd [dir]", "push the directory onto the stack and change to it"},
	"pwd":       {"pwd", "print the working directory"},
	"read":      {"read [-r] [-p prompt] [name ...]", "read a line from standard input into variables"},
	"readarray": {"readarray [-t] [array]", "read standard-input lines into an array (same as mapfile)"},
	"readonly":  {"readonly name[=value] ...", "mark variables as read-only"},
	"return":    {"return [n]", "return from a function or a sourced file"},
	"set":       {"set [-o option] [--] [argument ...]", "set shell options and positional parameters; set -o vi selects vi editing"},
	"shift":     {"shift [n]", "shift the positional parameters left by n"},
	"shopt":     {"shopt [-su] [option ...]", "set and unset bash-style shell options"},
	"source":    {"source file [arguments]", "run a file in the current shell (also spelled .)"},
	"test":      {"test expression", "evaluate a conditional expression; [ is the bracket spelling"},
	"trap":      {"trap [action condition ...]", "run an action on a signal, EXIT, or DEBUG"},
	"true":      {"true", "return success"},
	"type":      {"type name ...", "describe how each name would be run"},
	"typeset":   {"typeset [-aAfgilnrtux] [name[=value] ...]", "declare variables and give them attributes (also spelled declare)"},
	"ulimit":    {"ulimit [-HSacdefilmnpqrstuvx] [limit]", "report or set the resource limits the shell and its children get"},
	"unalias":   {"unalias [-a] name ...", "remove aliases"},
	"unset":     {"unset [-fv] name ...", "unset variables or functions"},
	"wait":      {"wait [id ...]", "wait for background jobs to finish"},

	// koi-native builtins.
	"bg":       {"bg [%job]", "resume a stopped job in the background"},
	"builtins": {"builtins", "list every builtin this session answers, grouped by origin"},
	"fc":       {"fc [-e ename] [-lnr] [first] [last] or fc -s [pat=rep] [command]", "list command history; only the listing half (-l) is implemented"},
	"fg":       {"fg [%job]", "resume a job in the foreground"},
	"help":     {"help [name]", "explain a builtin; koi commands also answer `<name> help`"},
	"jobs":     {"jobs", "list background and stopped jobs"},
	"disown":   {"disown [-ar] [jobspec ...]", "forget a background job"},
	"enable":   {"enable [-a] [-dnps] [-f filename] [name ...]", "enable or disable shell builtins"},
	"logout":   {"logout", "exit a login shell"},
	"kill":     {"kill [-signal] pid|%job ...", "send a signal to processes or jobs"},
	"newgrp":   {"newgrp", "not provided — it changes the real group id; use /usr/bin/newgrp"},
	"parallel": {"parallel [-j N] [--collect] [--fail-fast] -- cmd ... ::: inputs", "run a command over inputs in a bounded pool ({} substitutes)"},
	"plugins":  {"plugins", "inspect discovered tier-2 plugins and their capabilities"},
	"times":    {"times", "print user and system times for the shell and its children"},
	"umask":    {"umask [mode]", "set or print the file-creation mask"},

	// The session-scoped builtins (#618). See sessionBuiltins.
	"bind": {
		"bind [-X] [-m keymap] [-x keyseq:command] [-r keyseq] [keyseq:function]",
		"bind a key to a shell command or to a line-editor function",
	},
	"complete": {
		"complete [-abcdefgjkprsuv] [-DEI] [-o option] [-A action] [-G globpat] " +
			"[-W wordlist] [-F function] [-C command] [-X filterpat] [-P prefix] [-S suffix] [name ...]",
		"register how a command's arguments are completed",
	},
	"compopt": {
		"compopt [-o|+o option] [-DEI] [name ...]",
		"set or clear a completion's options — a registered one, or the one now running",
	},
	"history": {
		"history [-c] [-d offset] [n] or history -anrw [filename] or history -ps arg [arg ...]",
		"list the command history, or edit this session's copy of it",
	},

	// koi commands with no help subcommand of their own.
	"doctor":  {"doctor", "check the shell's moving parts and print the exact fix for each finding"},
	"explain": {"explain", "ask the configured AI provider why the last command failed"},
}

// sessionBuiltins are the bash builtins koi answers from *this package*
// rather than from the interpreter or the native registry (#618).
//
// They fell through both of `help`'s drift guards, which is why `type -t
// history` said `builtin` while `help history` denied there was such a
// thing. One guard walks `builtins.ShellBuiltins()` — the interpreter's
// table, which does not have them — and the other walks
// `callHandlerCommands`, which is koi's *own* commands and must not have
// them either, since these four are bash names and the `help` overview
// prints that list under "koi commands".
//
// So the list is declared here, beside the topics, and read from both
// directions by helpcmd_test.go. The half that cannot go stale is in
// `cmd/koi`: a name koi calls a builtin and does not refuse must have a
// topic, asked of a real shell, so the next one of these lands with a
// failing test rather than an undocumented builtin.
//
// What they still do *not* appear in is every other listing of builtins —
// `compgen -b`, `compgen -A enabled`, and the `help` overview's shell
// builtins line all read `builtins.ShellBuiltins()`. That is the same
// shape one layer along and is filed as #679 rather than absorbed here,
// because moving it means moving command-name completion with it.
var sessionBuiltins = []string{"bind", "complete", "compopt", "history"}

// sessionBuiltinNotes explain what koi's versions do, which is not what
// bash's do: they are the #159 ecosystem-inheritance builtins, so what a
// topic has to say is where the line editor and the shared history store
// put them. Printed under the one-liner by `help <name>`.
var sessionBuiltinNotes = map[string][]string{
	"bind": {
		"Only an interactive session has a line editor, so in a script",
		"`bind` accepts what it is given and does nothing.",
		"",
		"`-x` runs a shell command on a key, with READLINE_LINE and",
		"READLINE_POINT around it; `-r` and `-u` remove a binding; `-X`",
		"lists the `-x` ones; and a bare `keyseq:function` binds a",
		"readline function name koi's editor has an operation for.",
		"",
		"The listing forms, keymap selection and readline macros are",
		"accepted and ignored rather than refused: a tool's init script",
		"sets a dozen bindings and checks none of them, so failing on one",
		"would cost the eleven that work.",
	},
	"complete": {
		"Registrations are session-wide and are never written anywhere:",
		"they last as long as the shell. Outside an interactive session",
		"they are still recorded, so `complete -p` in a script reports",
		"what that script registered.",
		"",
		"`-F`, `-C`, `-W` and the `-A` actions generate; `-P`, `-S`, `-X`",
		"and `-G` are kept so that `eval \"$(complete -p cmd)\"` restores a",
		"spec whole, and are not applied yet.",
	},
	"compopt": {
		"The option names are `complete`'s own `-o` vocabulary, and the",
		"list is closed: one that is not in it is refused rather than",
		"accepted and dropped.",
		"",
		"With no names, the form means the completion that is *running*",
		"rather than a registered one, so it is only meaningful inside a",
		"completion function.",
	},
	"history": {
		"koi's history is a store shared live across sessions, not one",
		"list per shell, so the two halves of this builtin answer",
		"different things. Reading reports the store — the same entries",
		"the up-arrow and `fc -l` show. The first change (`-c`, `-d`,",
		"`-s`) takes a snapshot and everything after that works on the",
		"snapshot, so a script's edits are session-local and never touch",
		"what other shells can see.",
		"",
		"The file forms need $HISTFILE when no filename is given; koi has",
		"no default for it and says so rather than writing a file it will",
		"never read.",
	},
}

// helpSyntaxTopics are the shell *constructs* help answers for (#557):
// what someone reaches for after seeing `while` offered in a completion
// menu, and until now the answer was "no help topic for". They are a
// separate table from helpTopics on purpose — that one names commands
// and is checked against what the session dispatches, while every name
// here is grammar the parser reads and the interpreter runs, so its
// drift guard has to be a script rather than a lookup
// (cmd/koi/nativebuiltins_test.go runs one per topic).
//
// #269's rule applies unchanged: the answer is about *koi*. A construct
// koi refuses gets no topic, and the test asserts that in both
// directions so the list cannot quietly go stale. The wording is koi's
// own rather than bash's, deliberately: bash's help text is GPLv3 and
// this repository is MIT, which is the same reason #211's suite is
// never committed — so each construct is described here, never copied.
var helpSyntaxTopics = map[string]helpTopic{
	"!":         {"! pipeline", "run the pipeline and invert its exit status"},
	"%":         {"%job", "name a job for fg, bg, kill, wait and disown — %n, %%, %+, %-"},
	"(( ... ))": {"(( expression ))", "evaluate arithmetic; a non-zero value is success and zero is failure"},
	"[[ ... ]]": {"[[ expression ]]", "test a condition with pattern matching, =~, && and no word splitting"},
	"{ ... }":   {"{ commands ; }", "group commands in this shell, so one redirection covers all of them"},
	"case":      {"case word in [pattern | pattern) commands ;;] ... esac", "run the branch whose pattern the word matches (;;& falls through to the next)"},
	"coproc":    {"coproc [name] command", "run a command in the background with its input and output on a pair of descriptors"},
	"for":       {"for name [in words ...] ; do commands ; done", "run the body once per word, with name set to each in turn"},
	"for ((":    {"for (( init ; test ; step )) ; do commands ; done", "loop under arithmetic control — the C-style spelling of for"},
	"function":  {"function name { commands ; }, or name () { commands ; }", "define a function; declare -f prints one and unset -f removes it"},
	"if":        {"if commands ; then commands [; elif ...] [; else commands] ; fi", "run the then branch when the condition succeeds, otherwise the else branch"},
	"select":    {"select name [in words ...] ; do commands ; done", "print a numbered menu, read a choice into name (and REPLY), and repeat"},
	"time":      {"time [-p] pipeline", "run the pipeline and report real, user and system time on stderr"},
	"until":     {"until commands ; do commands ; done", "run the body until the condition succeeds"},
	"variables": {"variables", "the variables koi sets: PWD OLDPWD HOME PATH IFS SHLVL OPTIND RANDOM SECONDS LINENO EPOCHSECONDS EPOCHREALTIME HISTFILE BASH_VERSION BASH_VERSINFO KOI_VERSION — declare -p prints every one that is set"},
	"while":     {"while commands ; do commands ; done", "run the body while the condition succeeds"},
}

// helpSyntaxAliases are the spellings a person actually types for the
// three punctuation topics, since `help '[[ ... ]]'` is nobody's first
// guess. bash reaches them by prefix-matching its whole table, and
// matching only the *opening* form is what that behavior amounts to
// here — measured rather than assumed: bash answers `help '[['` and
// refuses `help ']]'`. They are deliberately absent from the compgen
// listing, because an alias is a way in rather than a topic and
// offering both spellings would list topics bash does not.
var helpSyntaxAliases = map[string]string{
	"((": "(( ... ))",
	"[[": "[[ ... ]]",
	"{":  "{ ... }",
}

// helpTopicFor resolves a name against both tables and the aliases,
// answering with the *listed* name: `help '[['` heads its entry
// `[[ ... ]]`, as bash does, so the name a completion offers is the one
// the answer names.
func helpTopicFor(name string) (string, helpTopic, bool) {
	if topic, ok := helpTopics[name]; ok {
		return name, topic, true
	}
	if topic, ok := helpSyntaxTopics[name]; ok {
		return name, topic, true
	}
	if canonical, ok := helpSyntaxAliases[name]; ok {
		if topic, ok := helpSyntaxTopics[canonical]; ok {
			return canonical, topic, true
		}
	}
	return name, helpTopic{}, false
}

// helpTopicNames is what `compgen -A helptopic` answers with: both
// tables, sorted together, aliases excluded.
func helpTopicNames() []string {
	names := make([]string, 0, len(helpTopics)+len(helpSyntaxTopics))
	for name := range helpTopics {
		names = append(names, name)
	}
	for name := range helpSyntaxTopics {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// runHelp answers `help [name ...]`. Rewrite names dispatch back into the
// handler chain via next; everything else prints here. Output stays plain
// text — help must read identically piped, scripted, and on a dumb
// terminal.
func runHelp(ctx context.Context, next interp.CallHandlerFunc, args []string) ([]string, error) {
	hc := interp.HandlerCtx(ctx)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		printHelpOverview(hc)
		return []string{"true"}, nil
	}
	// The single-name rewrite is the common case and hands the whole
	// command line over: `help config` runs `config help`, exit status
	// included.
	if len(args) == 1 && slices.Contains(helpRewrites, args[0]) {
		return next(ctx, []string{args[0], "help"})
	}
	ok := true
	for _, name := range args {
		switch listed, topic, found := helpTopicFor(name); {
		case slices.Contains(helpRewrites, name):
			// Only reachable with several names at once; pointing beats
			// splicing several full usage screens together.
			fmt.Fprintf(hc.Stdout, "%s: run `%s help` for its usage\n", name, name)
		case found:
			fmt.Fprintf(hc.Stdout, "%s: %s\n    %s\n", listed, topic.use, topic.desc)
			for _, line := range helpNotes[listed] {
				if line == "" {
					fmt.Fprintln(hc.Stdout)
					continue
				}
				fmt.Fprintf(hc.Stdout, "    %s\n", line)
			}
		default:
			hc.Errf("help: no help topic for %q — try `man %s`\n", name, name)
			ok = false
		}
	}
	if !ok {
		return []string{"false"}, nil
	}
	return []string{"true"}, nil
}

func printHelpOverview(hc interp.HandlerContext) {
	fmt.Fprintln(hc.Stdout, "help: help [name]")
	fmt.Fprintln(hc.Stdout, "    `help cd` explains a builtin, `help while` a piece of syntax;")
	fmt.Fprintln(hc.Stdout, "    koi commands also answer `<name> help`.")
	fmt.Fprintf(hc.Stdout, "\nkoi commands:\n  %s\n", strings.Join(callHandlerCommands, " "))
	fmt.Fprintf(hc.Stdout, "\nkoi builtins:\n  %s\n", strings.Join(builtins.Native(), " "))
	fmt.Fprintf(hc.Stdout, "\nshell builtins:\n  %s\n", strings.Join(builtins.ShellBuiltins(), " "))
	// Comma-separated where the other groups use spaces, because four of
	// these names contain a space of their own (`for ((`, `[[ ... ]]`) and
	// a space-joined listing would read as twice as many topics.
	fmt.Fprintf(hc.Stdout, "\nshell syntax:\n  %s\n",
		strings.Join(slices.Sorted(maps.Keys(helpSyntaxTopics)), ", "))
}
