package repl

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Bash's hook surface (#159).
//
// This is the structural advantage gish has over every previous new
// shell, and it is the one thing that has to be *right* rather than
// merely present. Every add-on in the ecosystem ships per-shell init
// scripts — starship carries eleven, zoxide nine, atuin seven — and a
// new shell has to be adopted by each one before it stops being a
// downgrade. nushell has more stars than fish and a quarter of the
// installs, and its own users describe booting zsh inside it.
//
// gish does not need to be adopted if it can run what those tools
// already emit for bash. That means the hook surface itself:
// PROMPT_COMMAND, PS0, and the DEBUG trap. Between them they carry
// starship, zoxide, atuin, direnv, mise and bash-preexec — which is in
// turn what a dozen more tools hook into.
//
// The hooks fire in the interactive loop rather than inside the
// interpreter: a hook is a property of "the shell is about to prompt"
// and "the shell is about to run your line", which are the loop's
// events, not the interpreter's. A script that installs a DEBUG trap
// therefore records it and never fires it — the same thing bash does
// with PROMPT_COMMAND in a script, and better than refusing the trap,
// which would make every tool's init script print an error.

// bashHooks is the session's hook state.
type bashHooks struct {
	// debugTrap is the DEBUG trap's body, run before each command line.
	debugTrap string
	// extdebug is `shopt -s extdebug`. Its load-bearing effect for hook
	// consumers is that a non-zero return from the DEBUG trap cancels
	// the command — which is how tools implement "don't run that".
	extdebug bool
}

var hooks = &bashHooks{}

// resetBashHooks clears hook state; one session, one set.
func resetBashHooks() { hooks = &bashHooks{} }

// trapCallHandler intercepts the DEBUG trap, which the interpreter does
// not implement (it answers "invalid signal specification", so every
// tool that installs one looks broken). Other signals pass through
// untouched, including in the same command: `trap x DEBUG EXIT` records
// the DEBUG half here and hands EXIT to the interpreter.
func trapCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "trap" || !slices.Contains(args, "DEBUG") {
			return next(ctx, args)
		}
		hc := interp.HandlerCtx(ctx)
		rest, handled := applyDebugTrap(hc.Stdout, args)
		switch {
		case !handled:
			return next(ctx, args)
		case rest != nil:
			return next(ctx, rest) // the non-DEBUG signals are still the interpreter's
		default:
			return []string{"true"}, nil
		}
	}
}

// applyDebugTrap records or clears the DEBUG trap.
//
// It returns the invocation the interpreter should still see: `trap x
// DEBUG EXIT` records the DEBUG half here and hands `trap x EXIT` on,
// while `trap x DEBUG` is handled entirely and the interpreter never
// sees a signal name it would reject.
func applyDebugTrap(out io.Writer, args []string) (rest []string, handled bool) {
	var action string
	var others []string
	haveAction, print := false, false
	for _, a := range args[1:] {
		switch {
		case a == "-p" && !haveAction:
			print = true
		case a == "--" && !haveAction:
			// the end-of-options separator carries no meaning here
		case !haveAction && !print:
			action, haveAction = a, true
		case a != "DEBUG":
			others = append(others, a)
		}
	}

	if print {
		if hooks.debugTrap != "" {
			fmt.Fprintf(out, "trap -- %s DEBUG\n", singleQuote(hooks.debugTrap))
		}
		return nil, true
	}
	if !haveAction {
		return nil, false // `trap DEBUG` alone is not a form bash defines
	}
	if action == "-" || action == "" {
		hooks.debugTrap = "" // reset to default: no trap
	} else {
		hooks.debugTrap = action
	}
	if len(others) == 0 {
		return nil, true
	}
	return append([]string{"trap", action}, others...), true
}

func singleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// doubleQuoteLiteral quotes s for an assignment's right-hand side,
// escaping everything that would otherwise expand.
//
// Double quotes rather than the usual `'\”` dance because the
// substrate mishandles that form *in an assignment* — `x='a'\”b'`
// yields `a\'b` there while `echo 'a'\”b'` is correct — and a command
// line containing an apostrophe is not exotic. Tracked as a substrate
// gap (#119); this spelling is correct regardless of which one is
// fixed.
func doubleQuoteLiteral(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(s) + `"`
}

// shoptCallHandler intercepts the shopt options the interpreter refuses
// but that init scripts set unconditionally. extdebug is the one that
// matters: bash-preexec and friends set it, and an error message in the
// middle of a tool's init is what makes a shell look unfinished.
func shoptCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "shopt" || !slices.Contains(args, "extdebug") {
			return next(ctx, args)
		}
		set := slices.Contains(args, "-s")
		unset := slices.Contains(args, "-u")
		remaining := []string{"shopt"}
		for _, a := range args[1:] {
			if a != "extdebug" {
				remaining = append(remaining, a)
			}
		}
		switch {
		case set:
			hooks.extdebug = true
		case unset:
			hooks.extdebug = false
		default:
			// A query: `shopt extdebug` reports its state, and its exit
			// status is the answer — that is how scripts test it.
			fmt.Fprintf(interp.HandlerCtx(ctx).Stdout, "extdebug\t%s\n", onOff(hooks.extdebug))
			if hooks.extdebug {
				return []string{"true"}, nil
			}
			return []string{"false"}, nil
		}
		if len(remaining) > 2 { // still has options and another name
			return next(ctx, remaining)
		}
		return []string{"true"}, nil
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// runPromptCommand runs PROMPT_COMMAND before drawing a prompt.
//
// Both forms are honored because both are in the wild: bash 5.1 made
// PROMPT_COMMAND an array, and tools emit whichever their author's bash
// had. An array's elements run in order, as bash runs them.
//
// Failures are ignored on purpose. PROMPT_COMMAND is where people put
// their least-careful code, and a shell that refuses to prompt because
// somebody's hook returned 1 is a shell nobody can recover in.
func runPromptCommand(ctx context.Context, runner *interp.Runner) {
	for _, cmd := range promptCommands(runner) {
		if strings.TrimSpace(cmd) == "" {
			continue
		}
		runHookSource(ctx, runner, cmd) //nolint:errcheck // a hook's failure is the hook's problem
	}
}

// promptCommands returns the PROMPT_COMMAND entries in order.
func promptCommands(runner *interp.Runner) []string {
	if v, ok := runner.Vars["PROMPT_COMMAND"]; ok && v.IsSet() {
		if v.Kind == expand.Indexed {
			return slices.Clone(v.List)
		}
		return []string{v.Str}
	}
	if s := runner.Env.Get("PROMPT_COMMAND").String(); s != "" {
		return []string{s}
	}
	return nil
}

// runPS0 prints PS0 after a line is read and before it runs — bash's
// "the command is starting" hook, used for timing banners and rulers.
func runPS0(ctx context.Context, runner *interp.Runner, out io.Writer) {
	ps0 := shellVar(runner, "PS0", "")
	if ps0 == "" {
		return
	}
	// PS0 goes through prompt expansion, which for gish means the same
	// escape set every other prompt uses. Command substitution inside it
	// is the common case (`$(date +%s)`), so it is expanded as shell
	// text rather than printed literally.
	expanded, err := expandPromptString(ctx, runner, ps0)
	if err != nil {
		return
	}
	fmt.Fprint(out, expanded)
}

// expandPromptString expands a prompt-ish string the way bash does for
// PS0/PS1: parameter and command substitution, no word splitting.
func expandPromptString(ctx context.Context, runner *interp.Runner, s string) (string, error) {
	var buf strings.Builder
	sub := runner.Subshell()
	interp.StdIO(nil, &buf, io.Discard)(sub) //nolint:errcheck // in-memory writer
	file, err := syntax.NewParser().Parse(strings.NewReader("printf %s "+singleQuoteExpandable(s)), "PS0")
	if err != nil {
		return "", err
	}
	if err := sub.Run(ctx, file); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// singleQuoteExpandable wraps s in double quotes for re-parsing, which
// keeps command and parameter substitution alive while preventing word
// splitting. Existing double quotes are escaped so the wrapper cannot
// be broken out of.
func singleQuoteExpandable(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// runDebugTrap fires the DEBUG trap before a command line runs, with
// BASH_COMMAND set to the line — which is exactly what bash-preexec and
// every preexec consumer reads.
//
// It reports whether the command should still run: under extdebug, a
// non-zero return from the trap cancels it, which is the semantics tools
// rely on to say "not that one".
func runDebugTrap(ctx context.Context, runner *interp.Runner, line string) bool {
	if hooks.debugTrap == "" {
		return true
	}
	// Assigned by running an assignment rather than by writing
	// runner.Vars: a hook body is almost always a function call, and a
	// function's variable lookups go through the interpreter's own scope
	// chain — poking the map left $BASH_COMMAND empty inside exactly the
	// functions that exist to read it.
	runHookSource(ctx, runner, "BASH_COMMAND="+doubleQuoteLiteral(line)) //nolint:errcheck // best effort
	defer runHookSource(ctx, runner, "unset BASH_COMMAND")               //nolint:errcheck // best effort
	err := runHookSource(ctx, runner, hooks.debugTrap)
	if !hooks.extdebug {
		return true
	}
	return exitCode(err) == 0
}

// runHookSource runs hook text in the session runner. Hooks define
// functions and set variables that must persist — bash-preexec's whole
// mechanism is a DEBUG trap that redefines things — so this is
// deliberately not a subshell.
func runHookSource(ctx context.Context, runner *interp.Runner, src string) error {
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "hook")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gish: hook:", err)
		return err
	}
	return runner.Run(ctx, file)
}
