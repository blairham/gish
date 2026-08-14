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
	"strings"

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
		if rerr := runner.Run(ctx, file); rerr != nil {
			if runner.Exited() {
				return rerr
			}
			if _, ok := errors.AsType[interp.ExitStatus](rerr); !ok {
				fmt.Fprintln(os.Stderr, "gish:", rerr)
			}
		}
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
