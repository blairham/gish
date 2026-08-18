package main

import (
	"fmt"
	"strings"
)

// A shell's argv is a POSIX contract, not a Go CLI (#217).
//
// koi parsed its own argv with the `flag` package, which models Go
// conventions: one flag per token, a flag's value is the next token, and
// options stop at the first one it does not recognize. A shell is spawned
// by other programs, and they expect bash's rules:
//
//   - clustered short options — `$SHELL -lc 'cmd'` is how tools spawn a
//     login shell that runs one command. `flag` reads that as an unknown
//     option named "lc" and exits 2.
//   - options *after* -c. The command string is an **operand**, not the
//     flag's argument, so `bash -c -l 'echo hi'` is a login shell running
//     `echo hi`. `flag` made "-l" the value of -c, so koi ran a command
//     named `-l` and answered 127 — which is exactly what Claude Code
//     spawns, and why every command it ran failed.
//   - `--` ends the options; everything after is an operand.
//
// So argv is parsed here by hand. The koi-specific long options keep
// working in both spellings (`-version` and `--version`), because that is
// what `flag` accepted and scripts may already use either.

// shellArgs is the parsed command line.
type shellArgs struct {
	command       string // -c's operand
	haveCommand   bool   // -c was given
	login         bool   // -l / --login
	interactive   bool   // -i
	version       bool   // --version
	remoteSession bool   // --remote-session
	sandbox       string // --sandbox profile
	rc            string // --rc file
	restore       string // --restore id
	operands      []string
}

// longOptions maps each long name to whether it takes a value. These are
// koi's own; the short set below is the POSIX one other programs use.
var longOptions = map[string]bool{
	"version":        false,
	"login":          false,
	"remote-session": false,
	"help":           false,
	"sandbox":        true,
	"rc":             true,
	"restore":        true,
}

// parseArgs reads a shell command line. The error is meant for the user:
// it is printed with the usage text.
func parseArgs(args []string) (shellArgs, error) {
	var out shellArgs
	i := 0
	for ; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		// A bare "-" is an operand (it means stdin), not an option.
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if takesValue, known := longOptions[name]; known {
			if takesValue && !hasValue {
				if i+1 >= len(args) {
					return out, fmt.Errorf("option --%s requires a value", name)
				}
				i++
				value = args[i]
			}
			if !takesValue && hasValue {
				return out, fmt.Errorf("option --%s takes no value", name)
			}
			switch name {
			case "version":
				out.version = true
			case "login":
				out.login = true
			case "remote-session":
				out.remoteSession = true
			case "help":
				return out, errHelp
			case "sandbox":
				out.sandbox = value
			case "rc":
				out.rc = value
			case "restore":
				out.restore = value
			}
			continue
		}
		// A cluster of short options: -l, -lc, -ilc.
		for _, r := range arg[1:] {
			switch r {
			case 'l':
				out.login = true
			case 'i':
				out.interactive = true
			case 'c':
				out.haveCommand = true
			default:
				return out, fmt.Errorf("unknown option %q in %q", string(r), arg)
			}
		}
	}
	out.operands = args[i:]
	if out.haveCommand {
		if len(out.operands) == 0 {
			return out, fmt.Errorf("option -c requires a command")
		}
		out.command, out.operands = out.operands[0], out.operands[1:]
	}
	return out, nil
}

// errHelp is --help, which is not a failure.
var errHelp = fmt.Errorf("help requested")

const usage = `usage: koi [options] [script [args…]]

  -c command        run command and exit; options may follow -c, and the
                    command is the first operand, as in bash
  -l, --login       act as a login shell (source profile files)
  -i                interactive: source the rc file even with -c
  --sandbox profile run every external command under a sandbox profile
                    (none opts out of the koi-agent default)
  --rc file         read startup settings from file
  --restore id      start in the directory of a recorded session
  --remote-session  this session was brought here by koi ssh
  --version         print version and exit

Short options cluster: koi -lc 'echo hi' is a login shell running one
command. Long options take either spelling: --version or -version.`
