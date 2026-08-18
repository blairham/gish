package repl

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

// A shell degrades; it does not crash (#217).
//
// The interpreter is a library, and a library's answer to an internal
// bug is to panic. A shell's cannot be: the process *is* the user's
// session, so taking it down over one bad expansion loses the terminal
// they were working in — or, when the panic comes from a profile file,
// stops them from getting a shell at all. That is not hypothetical:
//
//	$ koi -c 'x="  hi  "; echo "${x%%[![:space:]]*}"'
//	panic: regexp: Compile(`((?s)[^[:space:]\].*)$`): missing closing ]
//
// A negated POSIX class in a pattern-removal expansion makes the
// substrate's pattern→regexp translation emit an invalid regexp, which
// it compiles with MustCompile. It reached koi through ~/.profile —
// which `-l` sources — by way of a vendor's shell-integration block, so
// every login invocation died, which is exactly how a terminal emulator
// profile and a VS Code profile are configured to launch a shell.
//
// The gap itself belongs upstream (#119). What belongs here is the
// boundary: one bad line costs that line.

// safely runs fn, turning a panic into an error the caller reports the
// way it reports every other failure.
//
// what completes the sentence "internal error <what>", so it reads as a
// place: "running the command", "reading /etc/profile".
func safely(what string, fn func() error) (err error) {
	defer guard(what, &err)
	return fn()
}

// guard is the deferred half of safely, for callers that already have a
// named error return.
func guard(what string, err *error) {
	r := recover()
	if r == nil {
		return
	}
	msg := fmt.Sprintf("internal error %s: %v (this is a koi bug: %s)", what, r, issueURL)
	// The stack is what a bug report needs and what an interactive
	// prompt least wants scrolled past it, so it is opt-in. KOI_DEBUG
	// is read from the process environment rather than the session's:
	// whatever just panicked is not a runner to go asking questions of.
	if os.Getenv("KOI_DEBUG") != "" {
		msg += "\n" + indentStack(debug.Stack())
	} else {
		msg += "\nrun with KOI_DEBUG=1 to include the stack"
	}
	*err = fmt.Errorf("%s", msg)
}

const issueURL = "https://github.com/blairham/koi-shell/issues"

// indentStack keeps a stack trace visibly subordinate to the message.
func indentStack(stack []byte) string {
	return "\t" + strings.ReplaceAll(strings.TrimRight(string(stack), "\n"), "\n", "\n\t")
}
