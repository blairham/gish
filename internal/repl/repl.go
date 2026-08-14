// Package repl implements gish's read-eval loop on top of mvdan.cc/sh's
// POSIX/bash parser and interpreter.
//
// This is the walking skeleton: a line-oriented loop with no editing,
// highlighting, or completion yet. The interactive line editor (the
// zle-equivalent) replaces the plain prompt loop here; script and -c
// execution stay as they are.
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
)

const (
	prompt     = "gish$ "
	contPrompt = "> "
)

// Run starts the interactive loop on stdin and blocks until EOF or exit.
// The returned error is the session's exit status (an interp.ExitStatus)
// when the user ran exit, or a real I/O/parse failure.
func Run(ctx context.Context) error {
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
