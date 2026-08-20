package main

import (
	"fmt"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"
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
	noexec        bool   // -n: parse, report syntax errors, run nothing
	noRC          bool   // --norc
	noProfile     bool   // --noprofile
	noEditing     bool   // --noediting
	prettyPrint   bool   // --pretty-print
	version       bool   // --version
	remoteSession bool   // --remote-session
	sandbox       string // --sandbox profile
	rc            string // --rc file
	restore       string // --restore id
	// setFlags are the `set` options given in argv, in order, as `set`
	// itself would take them: ["-u"], ["-e", "-x"], ["-o", "posix"].
	// bash accepts any set option there and so must koi (#426).
	setFlags []string
	// shoptFlags are the `-O name` / `+O name` pairs, in order, as
	// `shopt` itself would take them: ["-s", "nullglob"] (#427).
	shoptFlags [][]string
	operands   []string
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
	// bash's long spelling of -r (#398).
	"restricted": false,
	// bash's --posix (#395).
	"posix": false,
	// bash's startup-file options (#531). A caller passing one of
	// these got a usage dump and no shell.
	"norc":         false,
	"noprofile":    false,
	"rcfile":       true,
	"noediting":    false,
	"pretty-print": false,
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
		// A bare "-" is an operand (it means stdin), not an option; so
		// is a bare "+". Otherwise a leading "+" is an option cluster
		// too, because that is how `set` spells turning one off and
		// bash takes the same spelling in argv (#426).
		if arg == "-" || arg == "+" {
			break
		}
		if !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "+") {
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
			case "restricted":
				out.setFlags = append(out.setFlags, "-r")
			case "posix":
				out.setFlags = append(out.setFlags, "-o", "posix")
			case "norc":
				out.noRC = true
			case "noprofile":
				out.noProfile = true
			case "rcfile":
				// bash's spelling of koi's --rc.
				out.rc = value
			case "noediting":
				out.noEditing = true
			case "pretty-print":
				out.prettyPrint = true
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
		// `-O name` / `+O name` are shopt's invocation form, the way an
		// option like nullglob is set before the first line runs
		// (#427). koi rejected the letter outright, and for the plus
		// form tried to open "+O" as a script.
		if arg == "-O" || arg == "+O" {
			state := "-s"
			if arg == "+O" {
				state = "-u"
			}
			if i+1 >= len(args) {
				// A bare -O lists the shopt table, as bash does.
				out.shoptFlags = append(out.shoptFlags, []string{})
				continue
			}
			i++
			out.shoptFlags = append(out.shoptFlags, []string{state, args[i]})
			continue
		}
		// `-o name` / `+o name`, the long spelling of a set option.
		if arg == "-o" || arg == "+o" {
			if i+1 >= len(args) {
				// bash with a bare -o prints the option table and
				// carries on; koi keeps that in the interpreter by
				// passing it through as `set -o`.
				out.setFlags = append(out.setFlags, arg)
				continue
			}
			i++
			out.setFlags = append(out.setFlags, arg, args[i])
			continue
		}
		// `+letters` is only ever a set option: koi's own flags have no
		// plus form, so the whole cluster goes to the interpreter.
		if strings.HasPrefix(arg, "+") {
			for _, r := range arg[1:] {
				if !interp.IsSetOptionFlag(byte(r)) {
					return out, fmt.Errorf("unknown option %q in %q", string(r), arg)
				}
			}
			out.setFlags = append(out.setFlags, arg)
			continue
		}
		// A cluster of short options: -l, -lc, -ilc, and any `set`
		// letter mixed in — `-euxc 'cmd'` is what CI files and Makefiles
		// write, and rejecting it made koi unusable as their $SHELL.
		var setLetters []rune
		for _, r := range arg[1:] {
			switch r {
			case 'l':
				out.login = true
			case 'i':
				out.interactive = true
			case 'c':
				out.haveCommand = true
			case 'n':
				// POSIX: read commands but do not execute them. Clustered
				// like the rest, because `sh -nc '…'` is a shape callers
				// write (#233).
				out.noexec = true
			default:
				// Not koi's own, so it is a `set` option or a typo. The
				// interpreter owns that table, and it is also the thing
				// that knows whether koi can honor the option — so the
				// letter is only validated here, never interpreted.
				if !interp.IsSetOptionFlag(byte(r)) {
					return out, fmt.Errorf("unknown option %q in %q", string(r), arg)
				}
				setLetters = append(setLetters, r)
			}
		}
		if len(setLetters) > 0 {
			out.setFlags = append(out.setFlags, "-"+string(setLetters))
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
  -n                parse and report syntax errors; run nothing (exit 2
                    on a syntax error, silent and 0 otherwise)
  --sandbox profile run every external command under a sandbox profile
                    (none opts out of the koi-agent default)
  --rc file         read startup settings from file
  --restore id      start in the directory of a recorded session
  --remote-session  this session was brought here by koi ssh
  --version         print version and exit

Short options cluster: koi -lc 'echo hi' is a login shell running one
command. Long options take either spelling: --version or -version.`
