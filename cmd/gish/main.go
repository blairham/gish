// Command gish is an interactive shell: zsh's interactive experience,
// bash's ubiquity, and a native, contract-first plugin system.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/repl"
)

// Stamped by -ldflags at build/release time; see Makefile and .goreleaser.yaml.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	command := flag.String("c", "", "run `command` and exit")
	loginFlag := flag.Bool("l", false, "act as a login shell (source profile files)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("gish %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}

	// Login invocation (#41): the -l flag, or argv[0] beginning with
	// '-' — how login(1) and sshd invoke a user's shell.
	login := *loginFlag || strings.HasPrefix(os.Args[0], "-")

	ctx := context.Background()
	var err error
	switch {
	case *command != "":
		err = repl.RunCommand(ctx, *command, login)
	case flag.NArg() > 0:
		err = repl.RunFile(ctx, flag.Arg(0), login)
	default:
		err = repl.Run(ctx, login)
	}
	if err == nil {
		return 0
	}
	// A nonzero exit status is the script's exit code, not a gish error.
	if status, ok := errors.AsType[interp.ExitStatus](err); ok {
		return int(status)
	}
	fmt.Fprintln(os.Stderr, "gish:", err)
	return 1
}
