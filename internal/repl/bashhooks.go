package repl

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/syntax"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/interp"
)

// Bash's hook surface (#159).
//
// This is the structural advantage koi has over every previous new
// shell, and it is the one thing that has to be *right* rather than
// merely present. Every add-on in the ecosystem ships per-shell init
// scripts — starship carries eleven, zoxide nine, atuin seven — and a
// new shell has to be adopted by each one before it stops being a
// downgrade. nushell has more stars than fish and a quarter of the
// installs, and its own users describe booting zsh inside it.
//
// koi does not need to be adopted if it can run what those tools
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
	// signalTraps holds handlers for the signals the interpreter does
	// not take. Only ERR and EXIT exist there, so `trap … INT` — which
	// direnv's own hook installs, and which a great many scripts use to
	// clean up — was answered with "invalid signal specification",
	// twice, at every startup.
	//
	// INT and WINCH are fired by the loop. The rest are recorded and
	// not fired: recording them costs nothing and removes the error,
	// while pretending to deliver a signal we do not catch would be a
	// lie a script could depend on.
	signalTraps map[string]string
}

var hooks = &bashHooks{signalTraps: map[string]string{}}

// resetBashHooks clears hook state; one session, one set.
func resetBashHooks() { hooks = &bashHooks{signalTraps: map[string]string{}} }

// firedSignals are the ones the loop actually delivers.
var firedSignals = map[string]bool{"INT": true, "WINCH": true}

// interpSignals are the two the interpreter implements itself.
var interpSignals = map[string]bool{"ERR": true, "EXIT": true}

// runSignalTrap fires a stored handler, if there is one.
func runSignalTrap(ctx context.Context, runner *interp.Runner, name string) {
	body := hooks.signalTraps[name]
	if body == "" || body == "-" {
		return
	}
	runHookSource(ctx, runner, body) //nolint:errcheck // a trap's failure is the trap's problem
}

// trapCallHandler is the *interactive* chain's trap seam: it claims the
// DEBUG trap so the loop can fire it as the preexec hook, once per
// command line, with extdebug's cancel semantics (#159). Other signals
// pass through untouched, including in the same command: `trap x DEBUG
// EXIT` records the DEBUG half here and hands EXIT to the interpreter.
func trapCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return trapHandler(next, true)
}

// scriptTrapCallHandler is the same seam for every non-interactive
// path — `-c`, a script file, piped stdin — where it does *not* claim
// DEBUG.
//
// It used to, and that was the bug (#268): the trap was recorded in
// hooks.debugTrap, which only the interactive loop ever reads, so a
// script's `trap … DEBUG` was accepted, silent, and never fired. Now the
// interpreter implements DEBUG itself, per command and with BASH_COMMAND
// set, so the honest thing is to get out of its way.
//
// The two paths therefore fire DEBUG at different granularities — per
// line here, per command there — and that is stated rather than hidden.
// bash is per-command everywhere; koi's interactive hook is the older
// shape that preexec consumers (Atuin, iTerm2, Kiro's integration) are
// wired to and that the ecosystem matrix pins, so it is not changed as a
// side effect of fixing the silent case.
func scriptTrapCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return trapHandler(next, false)
}

func trapHandler(next interp.CallHandlerFunc, ownDebug bool) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "trap" {
			return next(ctx, args)
		}
		args = normalizeSignalNames(args)
		if !ownDebug || !slices.Contains(args, "DEBUG") {
			// Only the interactive loop records anything here, and only
			// the two signals it delivers itself. The interpreter
			// implements real signal traps now (#350), so the script
			// paths get out of its way entirely — the same correction
			// #268 made for DEBUG, where recording a trap that nothing
			// would ever fire was the silent failure.
			if ownDebug {
				if rest, handled := recordSignalTraps(args); handled {
					if rest == nil {
						return []string{"true"}, nil
					}
					args = rest
				}
			}
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

// normalizeSignalNames strips the SIG prefix the interpreter rejects.
//
// bash accepts `trap … SIGINT` and `trap … INT` alike, and direnv's own
// hook uses the SIG form — which meant every direnv user's shell
// printed "invalid signal specification" twice at startup. The names
// are equivalent, so the prefix is simply removed.
func normalizeSignalNames(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if i > 0 && strings.HasPrefix(a, "SIG") && len(a) > 3 && strings.ToUpper(a) == a {
			a = strings.TrimPrefix(a, "SIG")
		}
		out = append(out, a)
	}
	return out
}

// recordSignalTraps takes the signals the interpreter would reject and
// records their handlers, leaving ERR and EXIT to it. It returns the
// remaining invocation, or nil when there is nothing left to pass on.
func recordSignalTraps(args []string) ([]string, bool) {
	rest := args[1:]
	// `trap -- '' SIGINT` is direnv's own spelling: the separator, then
	// an empty action, then the signal. Reading args[1] as the action
	// without skipping it recorded a handler named "--" and passed an
	// empty signal name on, which is the error direnv users saw.
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) < 2 {
		return nil, false // `trap`, `trap -p`: nothing to record
	}
	action := rest[0]
	var ours, theirs []string
	for _, sig := range rest[1:] {
		switch {
		case interpSignals[sig]:
			theirs = append(theirs, sig)
		case knownSignal(sig):
			ours = append(ours, sig)
		default:
			theirs = append(theirs, sig) // let the interpreter say why
		}
	}
	if len(ours) == 0 {
		return nil, false
	}
	for _, sig := range ours {
		if action == "-" || action == "" {
			delete(hooks.signalTraps, sig)
			continue
		}
		hooks.signalTraps[sig] = action
	}
	if len(theirs) == 0 {
		return nil, true
	}
	return append([]string{"trap", action}, theirs...), true
}

// knownSignal reports whether the interactive loop claims the signal:
// exactly the two it delivers itself (see firedSignals). Every other
// signal belongs to the interpreter, which arms and fires real traps for
// them (#350) — recording one here would silence it, which is the #268
// failure shape.
func knownSignal(name string) bool { return firedSignals[name] }

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
// ignorableShopts are bash options koi does not implement and does not
// need to: they configure a history file format, a redraw, or a
// completion cosmetic that koi handles its own way. Accepting them
// silently is deliberate — an init script sets a handful in a row and
// checks none of them, and an error in the middle of that is what makes
// a shell look unfinished.
//
// Options that change what a command *means* are deliberately not on
// this list, even when unimplemented: silently accepting `autocd` or
// `failglob` would make the shell behave differently from what the user
// just asked for, without saying so.
// The list shrank by fourteen names in #575, and the reason is the #566
// lesson one layer down: the interpreter now *records* the bit for the
// options whose behavior belongs to the line editor rather than to it, so
// stripping those requests here no longer avoided an error — it threw
// away a state change the interpreter would have made honestly, and
// `shopt -u histverify; shopt -p histverify` answered `-s`. What is left
// is only what the interpreter still refuses: an option koi keeps at a
// default it cannot leave, where an init script's request is silent
// rather than an error in the middle of somebody's tool setup.
var ignorableShopts = map[string]bool{
	"checkhash": true, "globasciiranges": true, "sourcepath": true,
	"interactive_comments": true, "login_shell": true, "shift_verbose": true,
}

// shoptCallHandler is the *interactive* chain's shopt seam: it claims
// extdebug so the loop's per-line DEBUG hook can honor its cancel
// semantics (#159), the way trapCallHandler claims DEBUG itself.
func shoptCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return shoptHandler(next, true)
}

// scriptShoptCallHandler is the same seam for the non-interactive paths,
// where extdebug passes through: the interpreter implements the option
// and its skip rule itself now (#355), and recording it in a hook only
// the interactive loop reads was the #268 silent-acceptance shape.
func scriptShoptCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return shoptHandler(next, false)
}

func shoptHandler(next interp.CallHandlerFunc, ownExtdebug bool) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		// `shopt -p` used to be rewritten here into a pipeline over the
		// query form, because the interpreter refused -p (#215). The
		// interpreter implements it now (#393), and the rewrite could
		// not carry -p's exit status through a pipeline anyway — it
		// always answered 0 where bash answers the option's state.
		if args[0] == "shopt" && shoptIsSetting(args) {
			if rest, handled := stripIgnorableShopts(args); handled {
				if rest == nil {
					return []string{"true"}, nil
				}
				args = rest
			}
		}
		if args[0] != "shopt" || !ownExtdebug || !slices.Contains(args, "extdebug") {
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

// shoptIsSetting reports whether this shopt call sets options rather
// than asking about them. The accept-and-ignore list answers a *request*
// -- an init script setting a handful of cosmetics in a row -- and has
// no business answering a question: dropping the name from `shopt -p
// checkjobs` printed nothing where bash prints the option, and made
// `shopt -q` answer 0 for an option that is off (#566). The interpreter
// holds the state and prints it correctly when asked for every option,
// so a query goes there.
func shoptIsSetting(args []string) bool {
	for _, a := range args[1:] {
		if a == "-s" || a == "-u" {
			return true
		}
	}
	return false
}

// stripIgnorableShopts removes the accepted-and-ignored names. It
// returns nil when nothing is left for the interpreter to do.
func stripIgnorableShopts(args []string) ([]string, bool) {
	rest := []string{"shopt"}
	names, dropped := 0, 0
	for _, a := range args[1:] {
		switch {
		case strings.HasPrefix(a, "-"):
			rest = append(rest, a)
		case ignorableShopts[a]:
			dropped++
		default:
			names++
			rest = append(rest, a)
		}
	}
	if dropped == 0 {
		return nil, false
	}
	if names == 0 {
		return nil, true
	}
	return rest, true
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
	// PS0 goes through prompt expansion, which for koi means the same
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
		fmt.Fprintln(os.Stderr, "koi: hook:", err)
		return err
	}
	return runner.Run(ctx, file)
}

// pendingSignals are traps whose signal arrived while something else
// owned the runner. They fire at the next prompt.
//
// Deferring is not a shortcut: SIGWINCH is delivered on a signal
// goroutine and an interrupt arrives while a command is mid-flight, and
// a runner entered from two goroutines at once is a data race with the
// user's whole session as its blast radius. The prompt is the moment
// the loop owns the interpreter, which is why every other hook fires
// there too.
var pendingSignals []string

func noteSignal(name string) {
	if hooks.signalTraps[name] == "" || !firedSignals[name] {
		return
	}
	if slices.Contains(pendingSignals, name) {
		return // one delivery per prompt, as with a coalesced resize
	}
	pendingSignals = append(pendingSignals, name)
}

func runPendingSignalTraps(ctx context.Context, runner *interp.Runner) {
	if len(pendingSignals) == 0 {
		return
	}
	names := pendingSignals
	pendingSignals = nil
	for _, name := range names {
		runSignalTrap(ctx, runner, name)
	}
}

// `declare -F name` — the standard "is this function defined?" test,
// used by fzf and by bash-completion constantly — cannot be intercepted
// here at all: the parser turns `declare` into a declaration clause
// before any handler sees it, so it never arrives as a call. It is a
// substrate gap rather than a koi one, recorded in the compat corpus
// and tracked in #119; the visible cost is that init scripts which
// probe for their own functions that way take their "not defined"
// branch.

// evalSeparatorCallHandler drops the `--` that ends eval's options.
//
// starship's own documented init line is `eval -- "$(starship init bash
// --print-full-init)"`, and the interpreter's eval does not know the
// separator: it ran `--` as a command and answered "command not found",
// so starship never initialized. bash treats `--` as end-of-options
// everywhere, and eval is where the ecosystem actually relies on it.
func evalSeparatorCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if len(args) > 2 && args[0] == "eval" && args[1] == "--" {
			args = append([]string{"eval"}, args[2:]...)
		}
		return next(ctx, args)
	}
}
