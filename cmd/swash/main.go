// Command swash is an interactive shell: zsh's interactive experience,
// bash's ubiquity, and a native, contract-first plugin system.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/swash/internal/repl"
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
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("swash %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}

	ctx := context.Background()
	var err error
	switch {
	case *command != "":
		err = repl.RunCommand(ctx, *command)
	case flag.NArg() > 0:
		err = repl.RunFile(ctx, flag.Arg(0))
	default:
		err = repl.Run(ctx)
	}
	if err == nil {
		return 0
	}
	// A nonzero exit status is the script's exit code, not a swash error.
	if status, ok := errors.AsType[interp.ExitStatus](err); ok {
		return int(status)
	}
	fmt.Fprintln(os.Stderr, "swash:", err)
	return 1
}
