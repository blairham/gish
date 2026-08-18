package repl

import (
	"context"
	"fmt"
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

// helpTopics covers every implemented shell builtin, every koi-native
// builtin, and the koi commands with no help subcommand of their own.
// helpcmd_test.go fails when a builtin ships without an entry here.
var helpTopics = map[string]helpTopic{
	// Shell builtins — the interpreter's implemented set.
	":":         {": [arguments]", "no effect: expand the arguments and return success"},
	"[":         {"[ expression ]", "evaluate a conditional expression — the bracket spelling of test"},
	"alias":     {"alias [name[=value] ...]", "define aliases, or print the defined ones"},
	"break":     {"break [n]", "exit a for, while, or until loop (n levels out)"},
	"builtin":   {"builtin name [arguments]", "run a shell builtin even when a function shadows its name"},
	"cd":        {"cd [dir]", "change the working directory (default $HOME); cd - returns to $OLDPWD"},
	"command":   {"command name [arguments]", "run a command bypassing shell functions and aliases"},
	"continue":  {"continue [n]", "start the next iteration of the enclosing loop"},
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
	"unalias":   {"unalias [-a] name ...", "remove aliases"},
	"unset":     {"unset [-fv] name ...", "unset variables or functions"},
	"wait":      {"wait [id ...]", "wait for background jobs to finish"},

	// koi-native builtins.
	"bg":       {"bg [%job]", "resume a stopped job in the background"},
	"builtins": {"builtins", "list every builtin this session answers, grouped by origin"},
	"fc":       {"fc -l [first [last]]", "list command history; the editing forms are not implemented"},
	"fg":       {"fg [%job]", "resume a job in the foreground"},
	"help":     {"help [name]", "explain a builtin; koi commands also answer `<name> help`"},
	"jobs":     {"jobs", "list background and stopped jobs"},
	"kill":     {"kill [-signal] pid|%job ...", "send a signal to processes or jobs"},
	"newgrp":   {"newgrp", "not provided — it changes the real group id; use /usr/bin/newgrp"},
	"parallel": {"parallel [-j N] [--collect] [--fail-fast] -- cmd ... ::: inputs", "run a command over inputs in a bounded pool ({} substitutes)"},
	"plugins":  {"plugins", "inspect discovered tier-2 plugins and their capabilities"},
	"times":    {"times", "print user and system times for the shell and its children"},
	"umask":    {"umask [mode]", "set or print the file-creation mask"},

	// koi commands with no help subcommand of their own.
	"doctor":  {"doctor", "check the shell's moving parts and print the exact fix for each finding"},
	"explain": {"explain", "ask the configured AI provider why the last command failed"},
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
		switch topic, found := helpTopics[name]; {
		case slices.Contains(helpRewrites, name):
			// Only reachable with several names at once; pointing beats
			// splicing several full usage screens together.
			fmt.Fprintf(hc.Stdout, "%s: run `%s help` for its usage\n", name, name)
		case found:
			fmt.Fprintf(hc.Stdout, "%s: %s\n    %s\n", name, topic.use, topic.desc)
		default:
			fmt.Fprintf(hc.Stderr, "help: no help topic for %q — try `man %s`\n", name, name)
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
	fmt.Fprintln(hc.Stdout, "    `help cd` explains a builtin; koi commands also answer `<name> help`.")
	fmt.Fprintf(hc.Stdout, "\nkoi commands:\n  %s\n", strings.Join(callHandlerCommands, " "))
	fmt.Fprintf(hc.Stdout, "\nkoi builtins:\n  %s\n", strings.Join(builtins.Native(), " "))
	fmt.Fprintf(hc.Stdout, "\nshell builtins:\n  %s\n", strings.Join(builtins.ShellBuiltins(), " "))
}
