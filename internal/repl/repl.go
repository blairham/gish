// Package repl implements gish's read-eval loop on top of mvdan.cc/sh's
// POSIX/bash parser and interpreter.
//
// Interactive terminals get the raw-mode line editor (internal/editor);
// piped stdin falls back to the plain line loop so `echo cmd | gish` and
// tests behave like a non-interactive shell. Script and -c execution are
// separate paths via RunReader.
package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/editor"
	"github.com/blairham/gish/internal/term"
)

const (
	prompt     = "gish$ "
	contPrompt = "> "
)

// Run starts the interactive loop on stdin and blocks until EOF or exit.
// The returned error is the session's exit status (an interp.ExitStatus)
// when the user ran exit, or a real I/O/parse failure.
func Run(ctx context.Context) error {
	if term.IsTerminal(os.Stdin) {
		return runEditor(ctx)
	}
	return runPlain(ctx)
}

// runEditor is the interactive path: the line editor owns the terminal
// between commands; the interpreter owns it while a command runs.
//
// Signal posture (see #3): an interactive shell must never die from the
// user's Ctrl-C or Ctrl-\. At the prompt those arrive as key events (raw
// mode); while a command runs, the terminal delivers them to the whole
// foreground process group — children included, which is what kills the
// child. gish catches its own copy via Notify (NOT Ignore: an ignored
// disposition would be inherited across exec and make children immune to
// Ctrl-C) and reacts by canceling the command context, which is what
// stops pure-builtin loops the kernel can't reach. SIGTSTP is left at
// its default until job control (#5).
func runEditor(ctx context.Context) error {
	runner, err := interp.New(interp.StdIO(os.Stdin, os.Stdout, os.Stderr))
	if err != nil {
		return err
	}
	ed := editor.New(term.NewTTY(os.Stdin, os.Stdout), os.Stdout, editor.Config{
		Prompt:     prompt,
		ContPrompt: contPrompt,
		AcceptWhen: acceptWhen,
	})
	parser := syntax.NewParser()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(sigs)

	for {
		line, err := ed.ReadCommand(ctx)
		switch {
		case errors.Is(err, editor.ErrInterrupted):
			continue // Ctrl-C: fresh prompt
		case errors.Is(err, io.EOF):
			return nil // Ctrl-D on an empty line
		case err != nil:
			return err
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		file, perr := parser.Parse(strings.NewReader(line), "gish")
		if perr != nil {
			fmt.Fprintln(os.Stderr, "gish:", perr)
			continue
		}

		drainSignals(sigs) // a signal from prompt-time must not cancel this command
		rerr := runInterruptible(ctx, runner, file, sigs)
		switch {
		case rerr == nil:
		case errors.Is(rerr, errInterrupted):
			// The command was interrupted, not the shell: fresh prompt.
			// Order matters — Runner.Exited() also reports true after a
			// cancellation, and the runner stays usable with state intact.
		case runner.Exited():
			return rerr
		default:
			if _, ok := errors.AsType[interp.ExitStatus](rerr); !ok {
				fmt.Fprintln(os.Stderr, "gish:", rerr)
			}
		}
	}
}

// errInterrupted marks a command run that ended because the user
// interrupted it — the shell continues, silently.
var errInterrupted = errors.New("command interrupted")

// runInterruptible runs one parsed command, canceling its context when
// SIGINT arrives so builtin-only loops stop too. External children get
// their signal directly from the terminal (same process group — that
// changes with job control, #5). SIGQUIT is swallowed for the shell and
// left to the kernel for the child.
func runInterruptible(ctx context.Context, runner *interp.Runner, file *syntax.File, sigs <-chan os.Signal) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-sigs:
				if sig == os.Interrupt {
					cancel()
				}
			case <-done:
				return
			}
		}
	}()
	err := runner.Run(runCtx, file)
	if err != nil && runCtx.Err() != nil && ctx.Err() == nil {
		return errInterrupted
	}
	return err
}

// drainSignals discards any signal that arrived while no command was
// running.
func drainSignals(sigs <-chan os.Signal) {
	select {
	case <-sigs:
	default:
	}
}

// acceptWhen reports whether text parses as a complete program. Syntax
// errors submit too — the interpreter reports them, matching how a shell
// treats a finished-but-wrong line.
func acceptWhen(text string) bool {
	_, err := syntax.NewParser().Parse(strings.NewReader(text), "gish")
	return err == nil || !syntax.IsIncomplete(err)
}

// runPlain is the non-TTY loop (piped stdin).
func runPlain(ctx context.Context) error {
	runner, err := interp.New(interp.StdIO(os.Stdin, os.Stdout, os.Stderr))
	if err != nil {
		return err
	}
	parser := syntax.NewParser()

	var exitErr error
	fmt.Fprint(os.Stdout, prompt)
loop:
	for stmts, err := range parser.InteractiveSeq(os.Stdin) {
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if parser.Incomplete() {
			fmt.Fprint(os.Stdout, contPrompt)
			continue
		}
		for _, stmt := range stmts {
			if err := runner.Run(ctx, stmt); err != nil {
				if runner.Exited() {
					exitErr = err
					break loop
				}
				// Nonzero statuses are ordinary interactive life; only
				// surface real interpreter errors.
				if _, ok := errors.AsType[interp.ExitStatus](err); !ok {
					fmt.Fprintln(os.Stderr, "gish:", err)
				}
			}
		}
		fmt.Fprint(os.Stdout, prompt)
	}
	fmt.Fprintln(os.Stdout)
	return exitErr
}

// RunCommand parses and runs src as a complete script (gish -c).
func RunCommand(ctx context.Context, src string) error {
	return RunReader(ctx, strings.NewReader(src), "gish -c")
}

// RunFile runs the script at path.
func RunFile(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return RunReader(ctx, f, path)
}

// RunReader parses and runs an entire script from r; name appears in
// error messages. Later opts override the default stdio, which keeps the
// core loop testable without touching the real terminal.
func RunReader(ctx context.Context, r io.Reader, name string, opts ...interp.RunnerOption) error {
	file, err := syntax.NewParser().Parse(r, name)
	if err != nil {
		return err
	}
	runner, err := interp.New(append(
		[]interp.RunnerOption{interp.StdIO(os.Stdin, os.Stdout, os.Stderr)},
		opts...,
	)...)
	if err != nil {
		return err
	}
	return runner.Run(ctx, file)
}
