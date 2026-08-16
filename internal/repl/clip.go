package repl

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/term"
)

// The `clip` builtin (#140): one clipboard command that works the same
// locally and over ssh.
//
// Today this is three commands depending on where you are — pbcopy on
// macOS, xclip or wl-copy on Linux, and nothing at all on a remote box
// unless you have set up forwarding. `clip` is one name for all of
// them, and over ssh it is the only one that works, because OSC 52 puts
// the text on the clipboard of the terminal you are sitting at rather
// than the machine the shell runs on.

const clipUsage = `usage: … | clip [-p]

  cat notes.md | clip        copy stdin to the clipboard
  clip -p                    copy to the X11 primary selection instead
  clip < file                same thing

Works over ssh: the text goes to the clipboard of the terminal you are
sitting at, not the machine the shell is running on. Needs a terminal
that acts on OSC 52 — ` + "`doctor`" + ` says whether yours does, and several
ship it switched off. Never emits anything without a terminal, so it is
safe in scripts (it just does nothing).`

// clipCallHandler intercepts `clip`, config-style.
func clipCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "clip" {
			return next(ctx, args)
		}
		return runClip(interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runClip(hc interp.HandlerContext, args []string) []string {
	primary := false
	for _, a := range args {
		switch a {
		case "-p", "--primary":
			primary = true
		case "help", "-h", "--help":
			fmt.Fprintln(hc.Stdout, clipUsage)
			return []string{"true"}
		default:
			fmt.Fprintf(hc.Stderr, "clip: unknown argument %q\n%s\n", a, clipUsage)
			return []string{"false"}
		}
	}

	text, err := io.ReadAll(hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "clip:", err)
		return []string{"false"}
	}
	if len(text) == 0 {
		return []string{"true"} // nothing to copy is not a failure
	}

	// The sequence goes to the terminal, which is os.Stdout — not
	// hc.Stdout, which in a pipeline is the next command's stdin. Writing
	// an escape sequence there would corrupt the pipeline instead of
	// reaching the terminal.
	out := os.Stdout
	if !term.ClipboardWritable(out) {
		// No terminal to write to. Silent success in a pipeline, one
		// line when someone typed it, so scripts never break and humans
		// are never confused.
		if term.IsTerminal(os.Stderr) {
			fmt.Fprintln(hc.Stderr, "clip: no terminal to copy through")
		}
		return []string{"true"}
	}

	set := term.SetClipboard
	if primary {
		set = term.SetPrimary
	}
	if err := set(out, strings.TrimRight(string(text), "\n")); err != nil {
		fmt.Fprintln(hc.Stderr, "clip:", err)
		return []string{"false"}
	}
	return []string{"true"}
}
