package repl

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/editor"
)

// The `bind` builtin (#159).
//
// fzf installs its Ctrl-T and Ctrl-R widgets with `bind -x`, atuin takes
// over history search the same way, and a great many rc files add
// one-key shortcuts with it. Without `bind`, every one of those init
// scripts prints an error and quietly does nothing — the shell looks
// broken at exactly the moment someone is evaluating it.
//
// What is honored:
//
//	bind -x '"\C-t": command'   run a shell command on that key
//	bind -r '"\C-t"'            remove a binding
//	bind -X                     list the -x bindings
//	bind '"\C-r": accept-line'  readline function bindings, where we
//	                            have the equivalent
//
// Everything else — `bind -p`, keymap selection, macros — is accepted
// and ignored rather than refused. A tool's init script sets a dozen
// bindings and checks none of them; failing loudly there costs the user
// the eleven that would have worked.

// editorRef reaches the live editor. The `bind` builtin runs inside the
// interpreter, which is created before the editor, so this is late-bound
// exactly like the runner accessor.
var editorRef func() *editor.Editor

func bindCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "bind" {
			return next(ctx, args)
		}
		hc := interp.HandlerCtx(ctx)
		return runBind(hc.Stdout, hc.Stderr, hc.ErrLocation, args[1:]), nil
	}
}

// errLoc is the caller's interp.HandlerContext.ErrLocation: bash
// locates a builtin's diagnostics and so does koi (#611), and this
// one takes writers rather than a handler context so it can be driven
// from a test with buffers.
func runBind(out, errOut io.Writer, errLoc string, args []string) []string {
	ed := currentEditor()
	if ed == nil {
		// Non-interactive: bindings have nowhere to live. Silence is
		// correct — a sourced rc sets them on the way past, and a script
		// that never reads a key does not care.
		return []string{"true"}
	}

	// Options are scanned rather than matched positionally, because the
	// real invocations do not put them where a naive parser expects:
	// fzf emits `bind -m emacs-standard -x '"\C-t": …'`, and reading
	// only args[0] finds -m and gives up on a binding that is right
	// there.
	var mode string
	var spec string
	list, remove := false, false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-m":
			if i+1 < len(args) {
				i++
				mode = args[i]
			}
		case "-x":
			if i+1 < len(args) {
				i++
				spec = args[i]
			}
		case "-r", "-u":
			remove = true
			if i+1 < len(args) {
				i++
				spec = args[i]
			}
		case "-X":
			list = true
		case "-f", "-q", "-V", "-S", "-p", "-P", "-l", "-s", "-v":
			// Listing and file-reading forms: accepted and ignored.
			if a == "-f" && i+1 < len(args) {
				i++
			}
		default:
			if !strings.HasPrefix(a, "-") && spec == "" {
				spec = a
			}
		}
	}
	// A binding for a keymap we do not have (vi-command while in emacs,
	// or the reverse) is not ours to install. Ignoring it quietly is
	// what bash does for a keymap that is not current.
	if mode != "" && !modeApplies(mode) {
		return []string{"true"}
	}

	switch {
	case list:
		bound := ed.BoundKeys()
		for _, seq := range slices.Sorted(maps.Keys(bound)) {
			fmt.Fprintf(out, "%s: %s\n", seq, bound[seq])
		}
		return []string{"true"}
	case remove && spec != "":
		if !ed.UnbindKey(spec) {
			fmt.Fprintf(errOut, "%sbind: %s: cannot parse key sequence\n", errLoc, spec)
			return []string{"false"}
		}
		return []string{"true"}
	case spec == "":
		return []string{"true"}
	}

	seq, command, ok := splitBinding(spec)
	if !ok {
		return []string{"true"} // not a binding form we recognize
	}
	if !isKeyCommand(args) {
		// A readline *function* binding (`"\C-r": reverse-search-history`)
		// or a macro. Function names we already bind to the same keys;
		// macros are readline's own editing language and are deliberately
		// not emulated — see docs/porting.md. Either way, success: a
		// tool's init sets a dozen bindings and checks none of them, and
		// failing loudly costs the user the eleven that would have worked.
		return []string{"true"}
	}
	if !ed.BindKeyCommand(seq, command) {
		// Reported, not fatal: an unsupported sequence should cost that
		// binding and nothing else.
		fmt.Fprintf(errOut, "%sbind: %s: unsupported key sequence\n", errLoc, seq)
		return []string{"false"}
	}
	return []string{"true"}
}

// isKeyCommand reports whether this invocation carried -x.
func isKeyCommand(args []string) bool { return slices.Contains(args, "-x") }

// modeApplies reports whether a `-m keymap` binding is for the keymap
// the editor is actually in. Unknown keymap names are treated as
// applying: refusing a binding because of an unfamiliar name is worse
// than installing one that turns out to be for another mode.
func modeApplies(mode string) bool {
	switch mode {
	case "vi", "vi-command", "vi-move", "vi-insert":
		return false // koi's vi mode has its own bindings (#163)
	default:
		return true
	}
}

// splitBinding splits readline's `"seq": action` form.
func splitBinding(s string) (seq, action string, ok bool) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", false
	}
	// A colon inside the quoted sequence belongs to the sequence.
	if q := strings.Index(s, `"`); q == 0 {
		if end := strings.Index(s[1:], `"`); end >= 0 {
			i = strings.Index(s[end+2:], ":")
			if i < 0 {
				return "", "", false
			}
			return s[:end+2], strings.TrimSpace(s[end+2+i+1:]), true
		}
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
}

func currentEditor() *editor.Editor {
	if editorRef == nil {
		return nil
	}
	return editorRef()
}

// keyCommandRunner runs a `bind -x` command with READLINE_LINE and
// READLINE_POINT set, and takes back whatever the command left in them.
//
// That pair is readline's whole contract with a bound command: fzf's
// widgets rewrite the line, atuin's replaces it outright, and a
// shortcut that only prints something leaves both alone. The terminal
// is already ceded by the editor when this runs, which is what lets
// fzf draw.
func keyCommandRunner(runner *interp.Runner) editor.KeyCommand {
	return func(command, line string, point int) (string, int, bool) {
		ctx := context.Background()
		runHookSource(ctx, runner, "READLINE_LINE="+doubleQuoteLiteral(line))     //nolint:errcheck // best effort
		runHookSource(ctx, runner, "READLINE_POINT="+strconv.Itoa(point))         //nolint:errcheck // best effort
		defer runHookSource(ctx, runner, "unset READLINE_LINE READLINE_POINT")    //nolint:errcheck // best effort
		if err := runHookSource(ctx, runner, command); err != nil && line == "" { //nolint:staticcheck // a failing widget still may have edited the line
			// A widget that failed may still have rewritten the line;
			// the values below are the authority, not the exit status.
			_ = err
		}
		newLine := shellVar(runner, "READLINE_LINE", line)
		newPoint := point
		if p, perr := strconv.Atoi(shellVar(runner, "READLINE_POINT", "")); perr == nil {
			newPoint = p
		}
		if newPoint > len([]rune(newLine)) {
			newPoint = len([]rune(newLine))
		}
		return newLine, newPoint, true
	}
}
