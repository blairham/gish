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

// dirs implements the builtin: `dirs [-clpv] [+N|-N]`.
func (r *Runner) dirs(args []string) exitStatus {
	full, perLine, verbose := false, false, false
	var index string
	for _, arg := range args {
		switch {
		case arg == "-c":
			// Clearing leaves the current directory, which is entry 0
			// rather than something the stack can drop.
			r.dirStack = r.dirStack[:1]
			r.dirStackSync()
			return exitStatus{}
		case arg == "-l":
			full = true
		case arg == "-p":
			perLine = true
		case arg == "-v":
			verbose, perLine = true, true
		case strings.HasPrefix(arg, "+") || strings.HasPrefix(arg, "-"):
			index = arg
		default:
			r.errf("dirs: %s: invalid option\n", arg)
			return exitStatus{code: 2}
		}
	}
	if index != "" {
		n, ok := r.dirStackIndex(index)
		if !ok {
			r.errf("dirs: %s: directory stack index out of range\n", index)
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
	change := true
	if len(args) > 0 && args[0] == "-n" {
		change = false
		args = args[1:]
	}
	r.dirStackSync()
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
	case 1:
		if n, ok := r.dirStackIndex(args[0]); ok {
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
		if strings.HasPrefix(args[0], "+") || strings.HasPrefix(args[0], "-") {
			if _, err := strconv.Atoi(args[0][1:]); err == nil {
				r.errf("pushd: %s: directory stack index out of range\n", args[0])
				return exitStatus{code: 1}
			}
		}
		if !change {
			// -n inserts below the current directory without moving:
			// the new entry lands at index 1, where pushing it on top
			// would leave a directory the shell is not in at index 0.
			r.dirStack = append(r.dirStack, "")
			copy(r.dirStack[2:], r.dirStack[1:])
			r.dirStack[1] = args[0]
			break
		}
		r.dirStack = append(r.dirStack, "")
		copy(r.dirStack[1:], r.dirStack)
		if code := r.changeDir(ctx, "pushd", args[0]); code != 0 {
			r.dirStack = r.dirStack[1:]
			return exitStatus{code: code}
		}
		r.dirStack[0] = r.Dir
	default:
		r.errf("pushd: too many arguments\n")
		return exitStatus{code: 2}
	}
	return r.dirs(nil)
}

// popd implements the builtin: `popd [-n] [+N|-N]`.
func (r *Runner) popd(ctx context.Context, args []string) exitStatus {
	change := true
	if len(args) > 0 && args[0] == "-n" {
		change = false
		args = args[1:]
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
	case 1:
		n, ok := r.dirStackIndex(args[0])
		if !ok {
			// bash separates a malformed argument from an in-range
			// one: the first is a usage error, the second a range one.
			if _, isIndex := parseStackIndex(args[0]); isIndex {
				if len(r.dirStack) < 2 {
					r.errf("popd: directory stack empty\n")
					return exitStatus{code: 1}
				}
				r.errf("popd: %s: directory stack index out of range\n", args[0])
				return exitStatus{code: 1}
			}
			r.errf("popd: %s: invalid argument\n", args[0])
			r.errf("popd: usage: popd [-n] [+N | -N]\n")
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
	default:
		r.errf("popd: too many arguments\n")
		return exitStatus{code: 2}
	}
	return r.dirs(nil)
}
