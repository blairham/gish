package interp

import (
	"context"
	"strconv"
	"strings"
)

// The directory stack (#390).
//
// bash's stack has no separate "bottom" slot for where the shell
// started: entry 0 *is* the current directory, which is why `cd` moves
// it and why `dirs` prints the cwd first. koi kept the cwd outside the
// stack and printed the slice backwards, so the bottom entry stayed
// frozen at the shell's startup directory — `cd /; pushd /usr; popd`
// returned to the wrong place — and DIRSTACK read reversed.
//
// The representation here is bash's: dirStack[0] is the current
// directory, and everything below is what pushd stacked under it.

// dirStackSync keeps entry 0 pointing at the current directory, which
// is what makes `cd` visible in `dirs` and in DIRSTACK.
func (r *Runner) dirStackSync() {
	if len(r.dirStack) == 0 {
		r.dirStack = append(r.dirStack, r.Dir)
		return
	}
	r.dirStack[0] = r.Dir
}

// dirStackIndex resolves a +N or -N stack argument to an index into
// dirStack, counting from the left for + and from the right for -.
func (r *Runner) dirStackIndex(arg string) (int, bool) {
	if len(arg) < 2 || (arg[0] != '+' && arg[0] != '-') {
		return 0, false
	}
	n, err := strconv.Atoi(arg[1:])
	if err != nil || n < 0 {
		return 0, false
	}
	if arg[0] == '-' {
		n = len(r.dirStack) - 1 - n
	}
	if n < 0 || n >= len(r.dirStack) {
		return 0, false
	}
	return n, true
}

// parseStackIndex reports whether arg has the +N or -N shape at all,
// which separates "out of range" from "not an index".
func parseStackIndex(arg string) (int, bool) {
	if len(arg) < 2 || (arg[0] != '+' && arg[0] != '-') {
		return 0, false
	}
	n, err := strconv.Atoi(arg[1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// dirStackEntry renders one entry the way `dirs` does: $HOME is
// abbreviated to ~ unless the listing asked for full paths.
func (r *Runner) dirStackEntry(dir string, full bool) string {
	if full {
		return dir
	}
	home := r.envGet("HOME")
	if home == "" || home == "/" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(dir, home+"/"); ok {
		return "~/" + rest
	}
	return dir
}

// signedWord reports whether a word carries a leading + or -, which is
// what makes it a candidate stack index rather than a directory.
//
// The length check is load-bearing: `pushd ""` and `popd ""` reach here,
// and indexing byte zero of an empty operand took the shell down with a
// panic. What bash does with those two is measured and different in
// each — pushd refuses a null directory when it would move and stacks
// the empty string under -n, popd reads it as `-0` — so neither is
// "no operand".
func signedWord(s string) bool {
	return s != "" && (s[0] == '+' || s[0] == '-')
}

// dirsUsage and friends are the usage lines bash prints under a bad
// argument. They are data a caller may match, so they are bash's byte
// for byte.
const (
	dirsUsage  = "dirs: usage: dirs [-clpv] [+N] [-N]\n"
	pushdUsage = "pushd: usage: pushd [-n] [+N | -N | dir]\n"
	popdUsage  = "popd: usage: popd [-n] [+N | -N]\n"
)

// dirs implements the builtin: `dirs [-clpv] [+N|-N]`.
func (r *Runner) dirs(args []string) exitStatus {
	full, perLine, verbose := false, false, false
	var index string
	for _, arg := range args {
		if arg == "--" {
			// `--` ends the options, and dirs takes no operands, so
			// everything after it is ignored: `dirs -- +1` is the plain
			// listing rather than entry 1, measured. koi read the `--`
			// itself as a stack index and answered "out of range".
			break
		}
		switch arg {
		case "-c":
			// Clearing leaves the current directory, which is entry 0
			// rather than something the stack can drop.
			r.dirStack = r.dirStack[:1]
			r.dirStackSync()
			return exitStatus{}
		case "-l":
			full = true
		case "-p":
			perLine = true
		case "-v":
			verbose, perLine = true, true
		default:
			if arg == "" || (arg[0] != '-' && arg[0] != '+') {
				r.errf("dirs: %s: invalid option\n", arg)
				r.rawErrf("%s", dirsUsage)
				return exitStatus{code: 2}
			}
			// Anything else beginning with a sign has to be a +N or -N.
			// The options do not cluster here — `dirs -lp` is bash's
			// `-lp: invalid number` rather than -l and -p — so a signed
			// word that is not a number gets the number's complaint.
			if _, ok := parseStackIndex(arg); !ok {
				r.errf("dirs: %s: invalid number\n", arg)
				r.rawErrf("%s", dirsUsage)
				return exitStatus{code: 2}
			}
			// The last index given wins: `dirs +1 +2` prints entry 2.
			index = arg
		}
	}
	if index != "" {
		n, ok := r.dirStackIndex(index)
		if !ok {
			// The sign is dropped: bash prints the number it parsed, so
			// `dirs +8` and `dirs -8` both complain about `8`.
			num, _ := parseStackIndex(index)
			r.errf("dirs: %d: directory stack index out of range\n", num)
			return exitStatus{code: 1}
		}
		r.outf("%s\n", r.dirStackEntry(r.dirStack[n], full))
		return exitStatus{}
	}
	for i, dir := range r.dirStack {
		entry := r.dirStackEntry(dir, full)
		switch {
		case verbose:
			r.outf("%2d  %s\n", i, entry)
		case perLine:
			r.outf("%s\n", entry)
		default:
			if i > 0 {
				r.out(" ")
			}
			r.out(entry)
		}
	}
	if !perLine {
		r.out("\n")
	}
	return exitStatus{}
}

// pushd implements the builtin: `pushd [-n] [+N|-N|dir]`.
func (r *Runner) pushd(ctx context.Context, args []string) exitStatus {
	change, literal := true, false
	for len(args) > 0 {
		if args[0] == "-n" {
			change = false
			args = args[1:]
			continue
		}
		if args[0] == "--" {
			// After `--` a `+1` is a *directory name* rather than a
			// stack index — measured, `pushd -- +1` is bash's `+1: No
			// such file or directory` — and with nothing after it the
			// bare-pushd swap is what is left. koi read the `--` as the
			// directory to change to.
			literal = true
			args = args[1:]
		}
		break
	}
	r.dirStackSync()
	if len(args) > 0 && !literal && args[0] != "-" && signedWord(args[0]) {
		// A bare `-` is the exception and is a directory, since `pushd -`
		// is `cd -`. Everything else carrying a sign must be a number,
		// and the check comes before the arity one: `pushd -x /tmp` is
		// bash's `-x: invalid number`, not "too many arguments".
		if _, ok := parseStackIndex(args[0]); !ok {
			r.errf("pushd: %s: invalid number\n", args[0])
			r.rawErrf("%s", pushdUsage)
			return exitStatus{code: 2}
		}
	}
	if len(args) > 1 {
		// bash answers 1 here rather than the usual usage 2, measured.
		r.errf("pushd: too many arguments\n")
		return exitStatus{code: 1}
	}
	switch len(args) {
	case 0:
		if !change {
			// `pushd -n` with nothing to push does nothing at all —
			// measured; swapping would move the shell, which is what
			// -n exists to prevent.
			return exitStatus{}
		}
		// Bare pushd exchanges the top two entries, which is the
		// idiom for bouncing between two directories.
		if len(r.dirStack) < 2 {
			r.errf("pushd: no other directory\n")
			return exitStatus{code: 1}
		}
		r.dirStack[0], r.dirStack[1] = r.dirStack[1], r.dirStack[0]
		if code := r.changeDir(ctx, "pushd", r.dirStack[0]); code != 0 {
			r.dirStack[0], r.dirStack[1] = r.dirStack[1], r.dirStack[0]
			return exitStatus{code: code}
		}
	default:
		if n, ok := r.dirStackIndex(args[0]); ok && !literal {
			// A stack argument *rotates* rather than pushing: the
			// named entry becomes the top and the ones above it move
			// underneath. koi read it as a filename and answered
			// "no such file or directory: +2" (#390).
			rotated := append(append([]string{}, r.dirStack[n:]...), r.dirStack[:n]...)
			r.dirStack = rotated
			if !change {
				break
			}
			if code := r.changeDir(ctx, "pushd", r.dirStack[0]); code != 0 {
				return exitStatus{code: code}
			}
			break
		}
		if !literal && args[0] != "-" && signedWord(args[0]) {
			// It parsed as a number above, so the only way here is out
			// of range.
			r.errf("pushd: %s: directory stack index out of range\n", args[0])
			return exitStatus{code: 1}
		}
		dir := args[0]
		if !change {
			// -n inserts below the current directory without moving:
			// the new entry lands at index 1, where pushing it on top
			// would leave a directory the shell is not in at index 0.
			r.dirStack = append(r.dirStack, "")
			copy(r.dirStack[2:], r.dirStack[1:])
			r.dirStack[1] = dir
			break
		}
		if dir == "-" {
			// `pushd -` is `cd -`: the previous directory, echoed the
			// way cd echoes it. koi answered "-: No such file or
			// directory", so the one-keystroke way back was unreachable
			// through pushd. Only on the moving path — `pushd -n -`
			// stacks the literal `-`, measured.
			dir = r.envGet("OLDPWD")
			r.outf("%s\n", dir)
		}
		r.dirStack = append(r.dirStack, "")
		copy(r.dirStack[1:], r.dirStack)
		if code := r.changeDir(ctx, "pushd", dir); code != 0 {
			r.dirStack = r.dirStack[1:]
			return exitStatus{code: code}
		}
		r.dirStack[0] = r.Dir
	}
	return r.dirs(nil)
}

// popd implements the builtin: `popd [-n] [+N|-N]`.
func (r *Runner) popd(ctx context.Context, args []string) exitStatus {
	change := true
	for len(args) > 0 {
		if args[0] == "-n" {
			change = false
			args = args[1:]
			continue
		}
		if args[0] == "--" {
			// popd takes no operand after `--`: `popd -- +8` pops the
			// top, measured, which is bash's own "this needs a fix to
			// work right" comment in builtins12.sub. koi read the `--`
			// as the index and refused it.
			args = nil
		}
		break
	}
	// Only the first operand is read; bash ignores the rest rather than
	// calling it too many arguments (`popd +1 +1` pops entry 1).
	if len(args) > 1 {
		args = args[:1]
	}
	if len(args) == 1 && args[0] == "" {
		// An empty operand is bash's `-0`, the entry at the *bottom*:
		// its sign character is the string terminator, which is not a
		// `+`, and bash counts from the bottom for anything else.
		args = []string{"-0"}
	}
	r.dirStackSync()
	switch len(args) {
	case 0:
		if len(r.dirStack) < 2 {
			r.errf("popd: directory stack empty\n")
			return exitStatus{code: 1}
		}
		if !change {
			// -n drops the entry *below* the current directory, so the
			// shell stays where it is and the stack still describes it
			// — measured, where dropping the top would leave entry 0
			// naming a directory the shell is not in.
			r.dirStack = append(r.dirStack[:1], r.dirStack[2:]...)
			break
		}
		r.dirStack = r.dirStack[1:]
		if code := r.changeDir(ctx, "popd", r.dirStack[0]); code != 0 {
			return exitStatus{code: code}
		}
	default:
		n, ok := r.dirStackIndex(args[0])
		if !ok {
			// bash separates three answers here, and koi had two: a
			// signed word that is not a number is an *invalid number*
			// (`popd -x`), an unsigned one is an invalid argument
			// (`popd dir`), and a well-formed index past the end is a
			// range error.
			if _, isIndex := parseStackIndex(args[0]); isIndex {
				if len(r.dirStack) < 2 {
					r.errf("popd: directory stack empty\n")
					return exitStatus{code: 1}
				}
				r.errf("popd: %s: directory stack index out of range\n", args[0])
				return exitStatus{code: 1}
			}
			what := "invalid argument"
			if signedWord(args[0]) {
				what = "invalid number"
			}
			r.errf("popd: %s: %s\n", args[0], what)
			r.rawErrf("%s", popdUsage)
			return exitStatus{code: 2}
		}
		if len(r.dirStack) < 2 {
			r.errf("popd: directory stack empty\n")
			return exitStatus{code: 1}
		}
		r.dirStack = append(r.dirStack[:n], r.dirStack[n+1:]...)
		// Removing an entry other than the current one changes the
		// listing and nothing else — only dropping entry 0 moves the
		// shell.
		if n != 0 || !change {
			break
		}
		if code := r.changeDir(ctx, "popd", r.dirStack[0]); code != 0 {
			return exitStatus{code: code}
		}
	}
	return r.dirs(nil)
}
