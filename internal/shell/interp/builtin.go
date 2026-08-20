// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package interp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"mvdan.cc/sh/v3/syntax"
)

// TODO: given the categories below, perhaps this should be more like:
//
//   func IsBuiltin(lang syntax.LangVariant, name string) bool
//
// or perhaps some API that also lets the user iterate through the builtins?
//
// Also, should we move this to the syntax package too?
// It's not a syntactical property strictly speaking,
// but it's also odd to require importing the interp package for it.

// IsBuiltin returns true if the given word is a POSIX Shell
// or Bash builtin.
func IsBuiltin(name string) bool {
	_, ok := builtinNames[name]
	return ok
}

// builtinNames is every builtin koi recognizes, and whether koi actually
// implements it. It is a map rather than the switch statement it replaced
// because three separate surfaces have to agree about what exists -- `type`,
// `compgen -b`, and running the thing -- and when they were free to disagree
// they did: `jobs` was reported by `type` as a builtin, omitted by
// `compgen -b`, and refused when run, which is the disagreement #302 is named
// after.
//
// A false value means the name is a real builtin that koi does not implement
// yet, so `compgen -b` leaves it out rather than advertising something that
// would refuse. The remaining six are all job control -- bg, fg, suspend and
// disown need a foreground/background notion koi does not have without `set
// -m` (#245), and enable and logout are interactive-shell management.
var builtinNames = map[string]bool{
	// POSIX Shell builtins, from section 1.d obtained in September 2025 from:
	// https://pubs.opengroup.org/onlinepubs/9699919799/utilities/V3_chap02.html#tag_18_09_01_01
	"alias":   true,
	"bg":      false,
	"cd":      true,
	"command": true,
	"false":   true,
	"fc":      false,
	"fg":      false,
	"getopts": true,
	"hash":    true,
	"jobs":    true,
	"kill":    false,
	"newgrp":  false,
	"pwd":     true,
	"read":    true,
	"true":    true,
	"umask":   false,
	"unalias": true,
	"wait":    true,

	// POSIX Shell special built-ins, obtained in September 2025 from:
	// https://pubs.opengroup.org/onlinepubs/9699919799/utilities/V3_chap02.html#tag_18_14
	"break":    true,
	":":        true,
	"continue": true,
	".":        true,
	"eval":     true,
	"exec":     true,
	"exit":     true,
	"export":   true, // NOTE: our parser treats this as a keyword
	"readonly": true, // NOTE: our parser treats this as a keyword
	"return":   true,
	"set":      true,
	"shift":    true,
	"times":    false,
	"trap":     true,
	"unset":    true,

	// Bash built-ins which are not present in POSIX, obtained in September 2025 from:
	// https://man.archlinux.org/man/bash.1.en#SHELL_BUILTIN_COMMANDS
	"source":    true,
	"bind":      false,
	"builtin":   true,
	"caller":    true,
	"compgen":   true,
	"complete":  false,
	"compopt":   false,
	"declare":   true, // NOTE: our parser treats this as a keyword
	"typeset":   true, // NOTE: our parser treats this as a keyword
	"dirs":      true,
	"disown":    false,
	"echo":      true, // TODO: surely this is POSIX? but why is it not in the main POSIX spec page?
	"enable":    false,
	"history":   false,
	"help":      false,
	"let":       true, // NOTE: our parser treats this as a keyword
	"local":     true,
	"logout":    false,
	"mapfile":   true,
	"readarray": true,
	"popd":      true,
	"printf":    true, // TODO: surely this is POSIX? but why is it not in the main POSIX spec page?
	"pushd":     true,
	"shopt":     true,
	"suspend":   false,
	"test":      true,
	"[":         true, // NOTE: an alias for "test", not explicitly listed
	"type":      true,
	"ulimit":    true,
}

// ImplementedBuiltins is the sorted list of builtins this interpreter can
// actually run, and UnimplementedBuiltins its complement: names it recognizes
// as builtins but refuses.
//
// They are exported because the layers above need the same answer. koi wraps
// this interpreter and adds builtins of its own, so "which builtins are there?"
// was being answered from a hand-maintained copy of this list in
// internal/builtins -- which is how `compgen -b` came to omit builtins that
// work and, before this, to omit `jobs` for a different reason than it was
// actually missing (#302). One list, derived, cannot drift from the dispatch
// it describes.
func ImplementedBuiltins() []string { return builtinsWhere(true) }

// UnimplementedBuiltins returns the recognized-but-refused builtins.
func UnimplementedBuiltins() []string { return builtinsWhere(false) }

func builtinsWhere(implemented bool) []string {
	names := make([]string, 0, len(builtinNames))
	for name, impl := range builtinNames {
		if impl == implemented {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// TODO: atoi is duplicated in the expand package.

// atoi is like [strconv.ParseInt](s, 10, 64), but it ignores errors and trims whitespace.
func atoi(s string) int64 {
	s = strings.TrimSpace(s)
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

type errBuiltinExitStatus exitStatus

func (e errBuiltinExitStatus) Error() string {
	return fmt.Sprintf("builtin exit status %d", e.code)
}

// Builtin allows [ExecHandlerFunc] implementations to execute any builtin,
// which can be useful for an exec handler to wrap or combine builtin calls.
//
// Note that a non-nil error may be returned in cases where the builtin
// alters the control flow of the runner, even if the builtin did not fail.
// For example, this is the case with `exit 0` or `return`.
func (hc HandlerContext) Builtin(ctx context.Context, args []string) error {
	if hc.kind != handlerKindExec {
		return fmt.Errorf("HandlerContext.Builtin can only be called via an ExecHandlerFunc")
	}
	exit := hc.runner.builtin(ctx, hc.Pos, args[0], args[1:])
	if exit != (exitStatus{}) {
		return errBuiltinExitStatus(exit)
	}
	return nil
}

func (r *Runner) builtin(ctx context.Context, pos syntax.Pos, name string, args []string) (exit exitStatus) {
	failf := func(code uint8, format string, args ...any) exitStatus {
		r.errf(format, args...)
		exit.code = code
		return exit
	}
	switch name {
	case ":", "true":
	case "false":
		exit.code = 1
	case "exit":
		switch len(args) {
		case 0:
			exit = r.lastExit
		case 1:
			n, err := strconv.Atoi(args[0])
			if err != nil {
				return failf(2, "invalid exit status code: %q\n", args[0])
			}
			exit.code = uint8(n)
		default:
			return failf(1, "exit cannot take multiple arguments\n")
		}
		exit.exiting = true
	case "set":
		if err := Params(args...)(r); err != nil {
			return failf(2, "set: %v\n", err)
		}
		r.updateExpandOpts()
	case "shift":
		n := 1
		switch len(args) {
		case 0:
		case 1:
			if n2, err := strconv.Atoi(args[0]); err == nil {
				n = n2
				break
			}
			fallthrough
		default:
			return failf(2, "usage: shift [n]\n")
		}
		if n >= len(r.Params) {
			r.Params = nil
		} else {
			r.Params = r.Params[n:]
		}
	case "unset":
		vars := true
		funcs := true
		// -n unsets the *reference* rather than what it points at, which
		// is the only way to remove a nameref at all (#277).
		byRef := false
	unsetOpts:
		for i, arg := range args {
			switch arg {
			case "-v":
				funcs = false
			case "-f":
				vars = false
			case "-n":
				byRef, funcs = true, false
			default:
				args = args[i:]
				break unsetOpts
			}
		}

		for _, arg := range args {
			// Without -n, unsetting a nameref unsets its *target* and
			// leaves the reference in place. That is bash's rule and it
			// is the opposite of what koi did, so `unset foo` removed the
			// nameref and kept the variable it pointed at — after which
			// every later use of the name was an ordinary variable and
			// the rest of a script drifted (#277).
			if _, _, isElem := cutElemSubscript(arg); vars && !byRef && !isElem {
				if vr := r.lookupVar(arg); vr.Kind == expand.NameRef && vr.Str != "" {
					arg = vr.Str
				}
			}
			if name, sub, ok := cutElemSubscript(arg); vars && ok {
				r.unsetElem(name, sub)
			} else if vars && r.lookupVar(arg).IsSet() {
				r.delVar(arg)
			} else if _, ok := r.Funcs[arg]; ok && funcs {
				delete(r.Funcs, arg)
			}
			if vars && arg == "GLOBIGNORE" {
				// bash turns dotglob off on `unset GLOBIGNORE` even when
				// the variable was never set (#375); delVar covers the
				// set case, this covers the rest.
				r.opts[optDotGlob] = false
				r.updateExpandOpts()
			}
		}
	case "echo":
		newline, doExpand := true, false
	echoOpts:
		for len(args) > 0 {
			switch args[0] {
			case "-n":
				newline = false
			case "-e":
				doExpand = true
			case "-E": // default
			default:
				break echoOpts
			}
			args = args[1:]
		}
		// One logical line, one write. Background jobs are goroutines
		// sharing this writer rather than separate processes with their
		// own fds, so an echo assembled from a write per argument lets
		// another job's output land mid-line -- "done:2done:3" (#301).
		// bash is atomic here because a short echo is a single write(2),
		// and building the line first is how we get the same guarantee.
		var line strings.Builder
		for i, arg := range args {
			if i > 0 {
				line.WriteString(" ")
			}
			if doExpand {
				arg, _, _ = expand.Format(r.ecfg, arg, nil)
			}
			line.WriteString(arg)
		}
		if newline {
			line.WriteString("\n")
		}
		r.out(line.String())
	case "printf":
		if len(args) == 0 {
			return failf(2, "usage: printf format [arguments]\n")
		}
		format, args := args[0], args[1:]
		// Accumulated for the same reason as echo above: a format that
		// recycles over its arguments would otherwise be one write per
		// cycle, and a concurrent job could interleave between them.
		var out strings.Builder
		for {
			s, n, err := expand.Format(r.ecfg, format, args)
			if err != nil {
				return failf(1, "%v\n", err)
			}
			out.WriteString(s)
			args = args[n:]
			if n == 0 || len(args) == 0 {
				break
			}
		}
		r.out(out.String())
	case "break", "continue":
		if !r.inLoop {
			return failf(0, "%s is only useful in a loop\n", name)
		}
		enclosing := &r.breakEnclosing
		if name == "continue" {
			enclosing = &r.contnEnclosing
		}
		switch len(args) {
		case 0:
			*enclosing = 1
		case 1:
			if n, err := strconv.Atoi(args[0]); err == nil {
				*enclosing = n
				break
			}
			fallthrough
		default:
			return failf(2, "usage: %s [n]\n", name)
		}
	case "pwd":
		// `set -o physical` makes resolving the default, which is what
		// -P asks for one call at a time.
		evalSymlinks := r.opts[optPhysical]
		for len(args) > 0 {
			switch args[0] {
			case "-L":
				evalSymlinks = false
			case "-P":
				evalSymlinks = true
			default:
				return failf(2, "invalid option: %q\n", args[0])
			}
			args = args[1:]
		}
		pwd := r.envGet("PWD")
		if evalSymlinks {
			var err error
			pwd, err = filepath.EvalSymlinks(pwd)
			if err != nil {
				exit.fatal(err) // perhaps overly dramatic?
				return exit
			}
		}
		r.outf("%s\n", pwd)
	case "cd":
		// -L and -P choose whether a symlinked path is kept as written
		// or resolved (#391). koi rejected both with a usage error and
		// exit 2, which cost whole suite files their content: a script
		// opening with `cd -P /` never changed directory at all.
		physical := false
		for len(args) > 0 && (args[0] == "-L" || args[0] == "-P" || args[0] == "--") {
			if args[0] == "--" {
				args = args[1:]
				break
			}
			physical = args[0] == "-P"
			args = args[1:]
		}
		var path string
		switch len(args) {
		case 0:
			path = r.envGet("HOME")
		case 1:
			path = args[0]

			// replicate the commonly implemented behavior of `cd -`
			// ref: https://www.man7.org/linux/man-pages/man1/cd.1p.html#OPERANDS
			if path == "-" {
				path = r.envGet("OLDPWD")
				r.outf("%s\n", path)
			} else if found, ok := r.cdPathLookup(ctx, path); ok {
				// A CDPATH hit prints where it landed, which is how a
				// script can tell the search happened.
				path = found
				r.outf("%s\n", path)
			}
		default:
			return failf(2, "cd: too many arguments\n")
		}
		if physical {
			if resolved, err := filepath.EvalSymlinks(r.absPath(path)); err == nil {
				path = resolved
			}
		}
		exit.code = r.changeDir(ctx, "cd", path)
	case "wait":
		anyJob := false
		pidVar := ""
		fp := flagParser{remaining: args}
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-n":
				anyJob = true
			case "-p":
				if pidVar = fp.value(); pidVar == "" {
					return failf(2, "wait: -p: option requires an argument\n")
				}
			default:
				return failf(2, "wait: invalid option %q\n", flag)
			}
		}
		args = fp.args()
		// bash leaves the variable *unset* unless a single job's status is
		// what comes back, so a script can tell "job N finished" from
		// "there was nothing to wait for" without reading $? twice.
		if pidVar != "" && r.lookupVar(pidVar).IsSet() {
			r.delVar(pidVar)
		}
		if anyJob {
			return r.waitAny(args, pidVar)
		}
		if len(args) == 0 {
			// Note that "wait" without arguments always returns exit status zero.
			for i := range r.bgProcs {
				<-r.bgProcs[i].done
				r.bgProcs[i].reaped = true
			}
			break
		}
		for _, arg := range args {
			i, ok := r.bgIndex(arg)
			if !ok {
				return failf(1, "wait: pid %s is not a child of this shell\n", arg)
			}
			<-r.bgProcs[i].done
			r.bgProcs[i].reaped = true
			exit = *r.bgProcs[i].exit
			if pidVar != "" {
				r.setVarString(pidVar, arg)
			}
		}
	case "builtin":
		if len(args) < 1 {
			break
		}
		if !IsBuiltin(args[0]) {
			exit.code = 1
			return exit
		}
		exit = r.builtin(ctx, pos, args[0], args[1:])
	case "type":
		anyNotFound := false
		mode := ""
		fp := flagParser{remaining: args}
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-a", "-f", "--help":
				return failf(3, "type: NOT IMPLEMENTED\n")
			case "-p", "-P", "-t":
				mode = flag
			default:
				return failf(2, "type: invalid option %q\n", flag)
			}
		}
		args := fp.args()
		for _, arg := range args {
			if mode == "-p" || mode == "-P" {
				if path, err := LookPathDir(r.Dir, r.writeEnv, arg); err == nil {
					r.outf("%s\n", path)
				} else {
					anyNotFound = true
				}
				continue
			}
			if syntax.IsKeyword(arg) {
				if mode == "-t" {
					r.out("keyword\n")
				} else {
					r.outf("%s is a shell keyword\n", arg)
				}
				continue
			}
			if als, ok := r.alias[arg]; ok && r.opts[optExpandAliases] {
				var buf bytes.Buffer
				if len(als.args) > 0 {
					printer := syntax.NewPrinter()
					printer.Print(&buf, &syntax.CallExpr{
						Args: als.args,
					})
				}
				if als.blank {
					buf.WriteByte(' ')
				}
				if mode == "-t" {
					r.out("alias\n")
				} else {
					r.outf("%s is aliased to `%s'\n", arg, &buf)
				}
				continue
			}
			if body, ok := r.Funcs[arg]; ok {
				if mode == "-t" {
					r.out("function\n")
				} else {
					// bash prints the definition under the verdict,
					// which is what `type` is for when the name is a
					// function: naming it without showing it leaves the
					// caller to run declare -f anyway (#386).
					r.outf("%s is a function\n", arg)
					r.printFuncDef(arg, body)
				}
				continue
			}
			if IsBuiltin(arg) {
				if mode == "-t" {
					r.out("builtin\n")
				} else {
					r.outf("%s is a shell builtin\n", arg)
				}
				continue
			}
			if path, err := LookPathDir(r.Dir, r.writeEnv, arg); err == nil {
				if mode == "-t" {
					r.out("file\n")
				} else {
					r.outf("%s is %s\n", arg, path)
				}
				continue
			}
			if mode != "-t" {
				r.errf("type: %s: not found\n", arg)
			}
			anyNotFound = true
		}
		if anyNotFound {
			exit.code = 1
		}
	case "hash":
		// TODO: implement. for now, having this as a no-op is better than nothing.
	case "eval":
		src := strings.Join(args, " ")
		// Read as bash reads (#276): what parsed before the error runs,
		// and only then is the error reported. `eval "$(tool init)"` is
		// the shape that matters — one construct koi cannot read at the
		// bottom of a generated hook used to discard the whole hook.
		stmts, perr := ParseAsRead(strings.NewReader(src), "")
		r.stmts(ctx, stmts)
		if perr != nil && !r.exit.exiting {
			return failf(SyntaxErrorStatus, "eval: %v\n", perr)
		}
		exit = r.exit
	case "source", ".":
		if len(args) < 1 {
			return failf(2, "%v: source: need filename\n", pos)
		}
		path, err := scriptFromPathDir(r.Dir, r.writeEnv, args[0])
		if err != nil {
			// If the script was not found in PATH or there was any error, pass
			// the source path to the open handler so it has a chance to look
			// at files it manages (eg: virtual filesystem), and also allow
			// it to look for the sourced script in the current directory.
			path = args[0]
		}
		f, err := r.open(ctx, path, os.O_RDONLY, 0, false)
		if err != nil {
			return failf(1, "source: %v\n", err)
		}
		defer f.Close()
		stmts, perr := ParseAsRead(f, path)

		// Keep the current versions of some fields we might modify.
		oldParams := r.Params
		oldSourceSetParams := r.sourceSetParams
		oldInSource := r.inSource

		// If we run "source file args...", set said args as parameters.
		// Otherwise, keep the current parameters.
		sourceArgs := len(args[1:]) > 0
		if sourceArgs {
			r.Params = args[1:]
			r.sourceSetParams = false
		}
		// We want to track if the sourced file explicitly sets the
		// parameters.
		r.sourceSetParams = false
		r.inSource = true // know that we're inside a sourced script.
		// A `source` is its own frame, so a library's own top level names
		// itself in BASH_SOURCE and a function it defines carries that
		// file with it. The line is where the `source` was written, which
		// is what BASH_LINENO reports for the frame below.
		// BASH_SOURCE reports the path as it was written, not as it was
		// resolved: `. ./lib.sh` names `./lib.sh`, which is what bash says
		// and what a library's own `dirname "${BASH_SOURCE[0]}"` expects.
		// Only a bare name — the PATH-searched form — reports where it was
		// actually found, since the name alone would not lead anyone back
		// to the file.
		sourceName := args[0]
		if !strings.ContainsRune(sourceName, filepath.Separator) {
			sourceName = path
		}
		popFrame := r.pushFrame(callFrame{
			name:     sourceFrameName,
			source:   sourceName,
			callLine: pos.Line(),
		})
		r.stmts(ctx, stmts)
		// A sourced file's return fires RETURN too, and unlike a
		// function it inherits the trap without needing "functrace".
		r.runReturnTrap(ctx)
		popFrame()

		// If we modified the parameters and the sourced file didn't
		// explicitly set them, we restore the old ones.
		if sourceArgs && !r.sourceSetParams {
			r.Params = oldParams
		}
		r.sourceSetParams = oldSourceSetParams
		r.inSource = oldInSource

		exit = r.exit
		exit.returning = false
		// The status of a sourced file that would not parse is the
		// syntax error's, not that of the last statement that did run —
		// and an `exit` inside it means bash never read far enough to
		// find the error at all.
		if perr != nil && !exit.exiting {
			return failf(SyntaxErrorStatus, "source: %v\n", perr)
		}
	case "[":
		if len(args) == 0 || args[len(args)-1] != "]" {
			return failf(2, "%v: [: missing matching ]\n", pos)
		}
		args = args[:len(args)-1]
		fallthrough
	case "test":
		parseErr := false
		p := testParser{
			rem: args,
			err: func(err error) {
				r.errf("%v: %v\n", pos, err)
				parseErr = true
			},
		}
		p.next()
		expr := p.classicTest("[", false)
		if parseErr {
			exit.code = 2
			return exit
		}
		exit.oneIf(r.bashTest(ctx, expr, true) == "")
	case "exec":
		// TODO: Consider unix.Exec, i.e. actually replacing
		// the process. It's in theory what a shell should do,
		// but in practice it would kill the entire Go process
		// and it's not available on Windows.
		argv0 := ""
		login, clearEnv := false, false
		fp := flagParser{remaining: args}
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-a":
				if len(fp.remaining) == 0 {
					return failf(2, "exec: -a: option requires an argument\n")
				}
				argv0 = fp.value()
			case "-l":
				login = true
			case "-c":
				clearEnv = true
			default:
				return failf(2, "exec: invalid option %q\n", flag)
			}
		}
		args := fp.args()
		if len(args) == 0 {
			// "exec" on its own keeps this statement's redirections open. Any
			// flags then have nothing to apply to, as in bash.
			r.keepRedirs = true
			break
		}
		if argv0 == "" {
			argv0 = args[0]
		}
		if login {
			// A login shell is told an argv[0] prefixed with "-".
			argv0 = "-" + argv0
		}
		r.exit.exiting = true
		if argv0 == args[0] {
			argv0 = "" // nothing to override
		}
		r.execWith(ctx, pos, argv0, clearEnv, args)
		exit = r.exit
	case "command":
		show := false
		fp := flagParser{remaining: args}
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-v":
				show = true
			default:
				return failf(2, "command: invalid option %q\n", flag)
			}
		}
		args := fp.args()
		if len(args) == 0 {
			break
		}
		if !show {
			if IsBuiltin(args[0]) {
				return r.builtin(ctx, pos, args[0], args[1:])
			}
			r.exec(ctx, pos, args)
			exit = r.exit
			return exit
		}
		last := uint8(0)
		for _, arg := range args {
			last = 0
			if r.Funcs[arg] != nil || IsBuiltin(arg) {
				r.outf("%s\n", arg)
			} else if path, err := LookPathDir(r.Dir, r.writeEnv, arg); err == nil {
				r.outf("%s\n", path)
			} else {
				last = 1
			}
		}
		exit.code = last
	case "dirs":
		return r.dirs(args)
	case "pushd":
		return r.pushd(ctx, args)
	case "popd":
		return r.popd(ctx, args)
	case "return":
		if !r.inFunc && !r.inSource {
			return failf(1, "return: can only be done from a func or sourced script\n")
		}
		switch len(args) {
		case 0:
		case 1:
			n, err := strconv.Atoi(args[0])
			if err != nil {
				return failf(2, "invalid return status code: %q\n", args[0])
			}
			exit.code = uint8(n)
		default:
			return failf(2, "return: too many arguments\n")
		}
		exit.returning = true
	case "read":
		var prompt string
		raw := false
		silent := false
		arrayName := ""
		delim := byte('\n')
		// maxChars is the count given to -n or -N; a negative value means that
		// the read is only stopped by the delimiter or by the end of the input.
		maxChars := -1
		// exactly is set by -N, which reads a fixed number of characters,
		// ignoring the delimiter and doing no field splitting.
		exactly := false
		// fd is the descriptor -u names; 0 is the shell's own stdin. haveFd
		// separates "the caller named a descriptor" from the default,
		// because only the first is worth a Bad file descriptor: a shell
		// with no stdin at all is the embedder's business, and readLine
		// already says so in those words.
		fd, haveFd := 0, false
		// timeout is -t. haveTimeout is separate because -t 0 is its own
		// thing — a test for whether input is waiting, reading nothing.
		var timeout time.Duration
		haveTimeout := false
		fp := flagParser{remaining: args}
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-s":
				silent = true
			case "-r":
				raw = true
			case "-a":
				// Note that bash takes the array name as this option's
				// argument, so further options may follow it.
				if len(fp.remaining) == 0 {
					return failf(2, "read: -a: option requires an argument\n")
				}
				arrayName = fp.value()
				if !syntax.ValidName(arrayName) {
					return failf(2, "read: invalid identifier %q\n", arrayName)
				}
			case "-p":
				prompt = fp.value()
				if prompt == "" {
					return failf(2, "read: -p: option requires an argument\n")
				}
			case "-d":
				// Note that an empty string is a valid delimiter, so we can't
				// use the empty return from value to detect a missing argument.
				if len(fp.remaining) == 0 {
					return failf(2, "read: -d: option requires an argument\n")
				}
				if val := fp.value(); val == "" {
					// Bash uses an ASCII NUL when given an empty string,
					// which is how "find -print0" input is consumed.
					delim = 0
				} else {
					delim = val[0]
				}
			case "-n", "-N":
				if len(fp.remaining) == 0 {
					return failf(2, "read: %s: option requires an argument\n", flag)
				}
				val := fp.value()
				n, err := strconv.Atoi(val)
				if err != nil || n < 0 {
					return failf(1, "read: %s: invalid number\n", val)
				}
				maxChars = n
				exactly = flag == "-N"
			case "-t":
				if len(fp.remaining) == 0 {
					return failf(2, "read: -t: option requires an argument\n")
				}
				val := fp.value()
				// Seconds, and fractional: `read -t 0.1` is how a script
				// polls without spinning. Note the status is 1 rather than
				// the usual 2 for a bad value, which is bash's.
				secs, err := strconv.ParseFloat(val, 64)
				if err != nil || secs < 0 {
					return failf(1, "read: %s: invalid timeout specification\n", val)
				}
				haveTimeout = true
				timeout = time.Duration(secs * float64(time.Second))
			case "-u":
				if len(fp.remaining) == 0 {
					return failf(2, "read: -u: option requires an argument\n")
				}
				val := fp.value()
				n, err := strconv.Atoi(val)
				if err != nil || n < 0 {
					return failf(1, "read: %s: invalid file descriptor specification\n", val)
				}
				fd, haveFd = n, true
			default:
				return failf(2, "read: invalid option %q\n", flag)
			}
		}

		args := fp.args()
		for _, name := range args {
			if !syntax.ValidName(name) {
				return failf(2, "read: invalid identifier %q\n", name)
			}
		}

		// After the names are validated, so `read 0ab` still complains
		// about the identifier rather than about a descriptor.
		src := r.fdReader(fd)
		if src == nil && haveFd {
			return failf(1, "read: %d: invalid file descriptor: Bad file descriptor\n", fd)
		}

		if prompt != "" {
			r.out(prompt)
		}

		// `-t 0` reads nothing at all: it answers whether input is waiting,
		// through the status alone. Anything else would consume the byte it
		// was asked about, which is worse than not implementing it — a
		// script that polls would eat its own input one character at a time.
		if haveTimeout && timeout == 0 {
			ready, err := readyToRead(src)
			if err != nil {
				return failf(1, "read: %v\n", err)
			}
			if !ready {
				exit.code = 1
			}
			return exit
		}

		var line []byte
		var err error
		var timedOut bool
		// -s only has an effect when reading from a terminal, as there is no
		// echo to suppress when the input is a pipe or a file. Note that we
		// must use the shell's stdin rather than the process's, as they differ
		// under a redirect and when the caller supplied its own via [StdIO].
		if f, ok := src.(*os.File); ok && silent && delim == '\n' && maxChars < 0 &&
			term.IsTerminal(int(f.Fd())) {
			line, err = term.ReadPassword(int(f.Fd()))
		} else {
			line, timedOut, err = r.readLine(ctx, src, raw, delim, maxChars, exactly, timeout)
		}
		switch {
		case arrayName != "":
			// Use -1 as max to get all fields without joining the last ones.
			values := expand.ReadFields(r.ecfg, string(line), -1, raw)
			r.setVar(arrayName, expand.Variable{
				Set:  true,
				Kind: expand.Indexed,
				List: values,
			})
		case exactly, len(args) == 0:
			// A bare "read" assigns the whole line to REPLY, and -N assigns
			// the characters it read to the first name given. Neither does any
			// trimming nor field splitting; both discard escapes unless raw.
			val := string(line)
			if !raw {
				val = unescapeRead(val)
			}
			name := shellReplyVar
			if len(args) > 0 {
				name = args[0]
			}
			r.setVarString(name, val)
			// Bash leaves any remaining names empty rather than unset.
			for _, name := range args[min(1, len(args)):] {
				r.setVarString(name, "")
			}
		default:
			values := expand.ReadFields(r.ecfg, string(line), len(args), raw)
			for i, name := range args {
				val := ""
				if i < len(values) {
					val = values[i]
				}
				r.setVarString(name, val)
			}
		}

		// We can get data back from readLine and an error at the same time, so
		// check err after we process the data. The same goes for a timeout:
		// whatever arrived before it is assigned, and only the status says
		// the read was cut short.
		switch {
		case timedOut:
			// bash reports a timeout as a status above 128, the way it
			// reports a signal — 128 + SIGALRM.
			exit.code = readTimeoutStatus
			return exit
		case err != nil:
			exit.code = 1
			return exit
		}

	case "getopts":
		if len(args) < 2 {
			return failf(2, "getopts: usage: getopts optstring name [arg ...]\n")
		}
		optind, _ := strconv.Atoi(r.envGet("OPTIND"))
		if optind-1 != r.optState.argidx {
			if optind < 1 {
				optind = 1
			}
			r.optState = getopts{argidx: optind - 1}
		}
		optstr := args[0]
		name := args[1]
		if !syntax.ValidName(name) {
			return failf(2, "getopts: invalid identifier: %q\n", name)
		}
		args = args[2:]
		if len(args) == 0 {
			args = r.Params
		}
		diagnostics := !strings.HasPrefix(optstr, ":")

		opt, optarg, done := r.optState.next(optstr, args)

		r.setVarString(name, string(opt))
		r.delVar("OPTARG")
		switch {
		case opt == '?' && diagnostics && !done:
			r.errf("getopts: illegal option -- %q\n", optarg)
		case opt == ':' && diagnostics:
			r.errf("getopts: option requires an argument -- %q\n", optarg)
		default:
			if optarg != "" {
				r.setVarString("OPTARG", optarg)
			}
		}
		if optind-1 != r.optState.argidx {
			r.setVarString("OPTIND", strconv.FormatInt(int64(r.optState.argidx+1), 10))
		}

		exit.oneIf(done)

	case "shopt":
		mode := ""
		posixOpts := false
		fp := flagParser{remaining: args}
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-s", "-u":
				mode = flag
			case "-o":
				posixOpts = true
			case "-p", "-q":
				return failf(2, "shopt: unsupported option %q\n", flag)
			default:
				return failf(2, "shopt: invalid option %q\n", flag)
			}
		}
		args := fp.args()
		if len(args) == 0 {
			if posixOpts {
				for i, opt := range &posixOptsTable {
					r.printOptLine(opt.name, setOptColumn, r.opts[i], true)
				}
			} else {
				for i, opt := range bashOptsTable {
					r.printOptLine(opt.name, shoptOptColumn, r.opts[len(posixOptsTable)+i], opt.supported)
				}
			}
			break
		}
		for _, arg := range args {
			opt, supported := (*bool)(nil), true
			if posixOpts {
				var po posixOpt
				opt, po = r.posixOptByName(arg)
				supported = po.supported
			} else {
				opt, supported = r.bashOptByName(arg)
			}
			if opt == nil {
				return failf(1, "shopt: invalid option name %q\n", arg)
			}

			switch mode {
			case "-s", "-u":
				if !supported {
					return failf(1, "shopt: unsupported option %q\n", arg)
				}
				*opt = mode == "-s"
			default: // ""
				r.printOptLine(arg, shoptOptColumn, *opt, supported)
			}
		}
		r.updateExpandOpts()

	case "alias":
		show := func(name string, als alias) {
			var buf bytes.Buffer
			if len(als.args) > 0 {
				printer := syntax.NewPrinter()
				printer.Print(&buf, &syntax.CallExpr{
					Args: als.args,
				})
			}
			if als.blank {
				buf.WriteByte(' ')
			}
			r.outf("alias %s='%s'\n", name, &buf)
		}

		if len(args) == 0 {
			for name, als := range r.alias {
				show(name, als)
			}
		}
	argsLoop:
		for _, arg := range args {
			name, src, ok := strings.Cut(arg, "=")
			if !ok {
				als, ok := r.alias[name]
				if !ok {
					r.errf("alias: %q not found\n", name)
					continue
				}
				show(name, als)
				continue
			}

			// TODO: parse any CallExpr perhaps, or even any Stmt
			parser := syntax.NewParser()
			var words []*syntax.Word
			for w, err := range parser.WordsSeq(strings.NewReader(src)) {
				if err != nil {
					r.errf("alias: could not parse %q: %v\n", src, err)
					continue argsLoop
				}
				words = append(words, w)
			}

			if r.alias == nil {
				r.alias = make(map[string]alias)
			}
			r.alias[name] = alias{
				args:  words,
				blank: strings.TrimRight(src, " \t") != src,
			}
		}
	case "unalias":
		for _, name := range args {
			delete(r.alias, name)
		}

	case "trap":
		fp := flagParser{remaining: args}
		callback := "-"
		list, print := false, false
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-l":
				list = true
			case "-p":
				print = true
			case "-":
				// default signal
			default:
				r.errf("trap: %q: invalid option\n", flag)
				r.errf("trap: usage: trap [-lp] [[arg] signal_spec ...]\n")
				exit.code = 2
				return exit
			}
		}
		args := fp.args()
		if list {
			r.printSignalNames()
			break
		}
		// `trap -p` and bare `trap` print the same thing; `-p` additionally
		// takes names to print, which is the form a script uses to save one
		// handler and restore it later.
		if print || len(args) == 0 {
			r.printTraps(args)
			break
		}
		// `trap SIG` and `trap - SIG` restore the default; `trap '' SIG`
		// ignores the signal. The fake traps have no default to restore,
		// so resetting and ignoring are the same operation for them —
		// for a real signal they are not (#350).
		reset := false
		switch len(args) {
		case 1:
			reset = true
		default:
			callback = args[0]
			args = args[1:]
			if callback == "-" {
				callback, reset = "", true
			}
		}
		if callback == "-" {
			callback = ""
		}
		for _, arg := range args {
			// Specs are case-insensitive, and 0 is EXIT (#351):
			// `trap 'rm -f $tmp' 0` is the cleanup idiom in decades of
			// scripts.
			spec := strings.ToUpper(arg)
			if spec == "0" {
				spec = "EXIT"
			}
			switch spec {
			case "ERR":
				r.callbackErr, r.listed.err = callback, callback
				// Installing the trap rebases the inheritance rule to
				// here: a trap set inside a subshell or a function fires
				// for failures in that scope (#354) — "not inherited"
				// restricts a *parent's* trap, not the one this scope
				// just set. Leaving depth alone made `(trap 'echo e'
				// ERR; false)` silent. Function returns restore their
				// caller's depth, so the rebase does not leak upward.
				r.errTrapDepth = 0
			case "EXIT":
				r.callbackExit, r.listed.exit = callback, callback
				r.callbackExitLine = pos.Line()
			case "DEBUG":
				r.callbackDebug, r.listed.debug = callback, callback
			case "RETURN":
				// Setting it here also makes it reachable here: `trap`
				// installs the handler for the context it is run in, so a
				// function that sets its own RETURN trap fires it even
				// though entering that function had turned inheritance
				// off. That is what makes the cleanup idiom work.
				r.callbackReturn, r.listed.ret = callback, callback
				r.callbackReturnLine = pos.Line()
				r.returnTrapOff = false
			default:
				name, sig, ok := lookupSignal(arg)
				if !ok {
					return failf(1, "trap: %s: invalid signal specification\n", arg)
				}
				r.setSignalTrap(name, sig, callback, reset, pos.Line())
			}
		}

	case "readarray", "mapfile":
		dropDelim := false
		delim := "\n"
		fp := flagParser{remaining: args}
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-t":
				// Remove the delim from each line read
				dropDelim = true
			case "-d":
				if len(fp.remaining) == 0 {
					return failf(2, "%s: -d: option requires an argument\n", name)
				}
				delim = fp.value()
				if delim == "" {
					// Bash sets the delim to an ASCII NUL if provided with an empty
					// string.
					delim = "\x00"
				}
			default:
				return failf(2, "%s: invalid option %q\n", name, flag)
			}
		}

		args := fp.args()
		var arrayName string
		switch len(args) {
		case 0:
			arrayName = "MAPFILE"
		case 1:
			if !syntax.ValidName(args[0]) {
				return failf(2, "%s: invalid identifier %q\n", name, args[0])
			}
			arrayName = args[0]
		default:
			return failf(2, "%s: Only one array name may be specified, %v\n", name, args)
		}

		var vr expand.Variable
		vr.Kind = expand.Indexed
		scanner := bufio.NewScanner(r.stdin)
		scanner.Split(mapfileSplit(delim[0], dropDelim))
		for scanner.Scan() {
			vr.List = append(vr.List, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			return failf(2, "%s: unable to read, %v\n", name, err)
		}
		r.setVar(arrayName, vr)

	case "compgen":
		// Only the actions which enumerate what the shell itself knows are
		// implemented; the rest are refused rather than silently answering
		// nothing, which is the failure mode worth avoiding for a builtin
		// whose whole job is to answer "what exists?".
		action := ""
		fp := flagParser{remaining: args}
		for fp.more() {
			switch flag := fp.flag(); flag {
			case "-A":
				if len(fp.remaining) == 0 {
					return failf(2, "compgen: -A: option requires an argument\n")
				}
				action = fp.value()
			case "-a":
				action = "alias"
			case "-v":
				action = "variable"
			case "-b":
				action = "builtin"
			default:
				return failf(2, "compgen: %q: NOT IMPLEMENTED flag\n", flag)
			}
		}
		var names []string
		switch action {
		case "function":
			for name := range r.Funcs {
				names = append(names, name)
			}
		case "alias":
			for name := range r.alias {
				names = append(names, name)
			}
		case "builtin":
			// Only the builtins koi implements. Listing one it would
			// refuse is the disagreement this is here to end (#302):
			// "what exists?" and "what can I call?" have to be the
			// same answer for a builtin whose whole job is the first
			// question.
			names = append(names, ImplementedBuiltins()...)
		case "variable":
			r.writeEnv.Each(func(name string, vr expand.Variable) bool {
				if vr.IsSet() {
					names = append(names, name)
				}
				return true
			})
		case "":
			return failf(2, "compgen: an action is required, such as -A function\n")
		default:
			return failf(2, "compgen: -A %q: NOT IMPLEMENTED action\n", action)
		}
		// A word operand is a prefix to match, not a pattern.
		if rest := fp.args(); len(rest) > 0 {
			prefix := rest[0]
			names = slices.DeleteFunc(names, func(name string) bool {
				return !strings.HasPrefix(name, prefix)
			})
		}
		slices.Sort(names)
		names = slices.Compact(names)
		for _, name := range names {
			r.outf("%s\n", name)
		}
		if len(names) == 0 {
			// Bash reports no matches with a non-zero status.
			exit.code = 1
		}

	case "caller":
		// `caller` is the frame stack as a builtin (#250, #266): the same
		// three fields FUNCNAME, BASH_SOURCE and BASH_LINENO expose, read
		// one frame down from the argument.
		//
		// It answers by *status* when there is no such frame, which is what
		// callers act on: `caller 0` at the top level of a script is how an
		// error helper asks "was I called from a function?" and expects a
		// non-zero answer rather than a diagnostic.
		frames := r.baseFrames()
		depth := 0
		if len(args) > 0 {
			n, err := strconv.Atoi(args[0])
			if err != nil || n < 0 {
				// bash separates these: a negative number is read as an
				// option, anything else as a bad number. Both exit 2 with
				// the usage line, which is what a caller sees.
				what := "invalid number"
				if strings.HasPrefix(args[0], "-") {
					what = "invalid option"
				}
				r.errf("caller: %s: %s\ncaller: usage: caller [expr]\n", args[0], what)
				exit.code = 2
				break
			}
			depth = n
		}
		// Outside a function there is nothing to report, whatever the
		// depth: `caller` exists to name the caller of a function.
		if !r.inFunction() || depth >= len(frames) {
			exit.code = 1
			break
		}
		if len(args) == 0 {
			// Bare `caller` prints the line and the file only, and it does
			// not need a frame above to exist — bash prints its literal
			// "NULL" for the file instead, which is what `-c` produces.
			src := ""
			if depth+1 < len(frames) {
				src = frames[depth+1].source
			}
			r.outf("%d %s\n", frames[depth].callLine, orNull(src))
			break
		}
		// `caller N` names a function, so the frame above has to be there.
		if depth+1 >= len(frames) {
			exit.code = 1
			break
		}
		up := frames[depth+1]
		r.outf("%d %s %s\n", frames[depth].callLine, up.name, orNull(up.source))

	case "ulimit":
		return r.ulimitBuiltin(args)

	case "jobs":
		return r.jobsBuiltin(args)

	case "declare", "typeset", "local", "export", "readonly", "nameref":
		// The parser produces a DeclClause when one of these words sits
		// at command position, so this path runs only when something kept
		// it from being a keyword — a prefix assignment is the case
		// bash's own suite exercises (`ref=xxx typeset -p ref var`,
		// nameref14.sub), and it answered "unsupported builtin" (#277).
		// The args arrive already expanded; wrapping each in a literal
		// Assign is exactly what flattenAssigns builds for a naked word,
		// so no value is expanded twice.
		assigns := make([]*syntax.Assign, 0, len(args))
		for _, field := range args {
			as := &syntax.Assign{}
			nm, val, ok := strings.Cut(field, "=")
			as.Name = &syntax.Lit{Value: nm}
			if !ok {
				as.Naked = true
			} else {
				as.Value = &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: val}}}
			}
			assigns = append(assigns, as)
		}
		// declClause reports through r.exit, the way the DeclClause node
		// does; run it against a clean status and hand the result back
		// through the builtin contract.
		oldExit := r.exit
		r.exit = exitStatus{}
		r.declClause(name, assigns)
		exit, r.exit = r.exit, oldExit
		return exit

	default:
		return failf(2, "%s: unsupported builtin\n", name)
	}
	return exit
}

// orNull is bash's spelling for a frame whose file is not known.
func orNull(s string) string {
	if s == "" {
		return "NULL"
	}
	return s
}

// mapfileSplit returns a suitable Split function for a [bufio.Scanner];
// the code is mostly stolen from [bufio.ScanLines].
func mapfileSplit(delim byte, dropDelim bool) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := bytes.IndexByte(data, delim); i >= 0 {
			// We have a full newline-terminated line.
			if dropDelim {
				return i + 1, data[0:i], nil
			} else {
				return i + 1, data[0 : i+1], nil
			}
		}
		// If we're at EOF, we have a final, non-terminated line. Return it.
		if atEOF {
			return len(data), data, nil
		}
		// Request more data.
		return 0, nil, nil
	}
}

// setOptColumn is the width bash pads a `set -o` name to before the tab.
// Measured from bash 5.3 rather than chosen — a listing is something
// scripts cut fields out of.
//
// `shopt` pads too, to twenty, and koi deliberately does not follow it
// there. The width is not stable across bash versions the way this one is:
// matching 5.3's shopt makes koi differ from the 3.2 that ships on macOS,
// which is what the CI runner has, so the choice is which bash to be wrong
// against rather than whether. That difference is recorded as a known gap
// in the builtins matrix and belongs to `shopt` rather than to #245.
const (
	setOptColumn   = 15
	shoptOptColumn = 0 // no padding; see above
)

func (r *Runner) printOptLine(name string, column int, enabled, supported bool) {
	state := r.optStatusText(enabled)
	if supported {
		r.outf("%-*s\t%s\n", column, name, state)
		return
	}
	r.outf("%-*s\t%s\t(%q not supported)\n", column, name, state, r.optStatusText(!enabled))
}

// unescapeRead drops the backslashes which escape another character, as "read"
// does when its -r option is not given.
func unescapeRead(val string) string {
	var sb strings.Builder
	esc := false
	for i := range len(val) {
		if val[i] == '\\' && !esc {
			esc = true
			continue
		}
		sb.WriteByte(val[i])
		esc = false
	}
	return sb.String()
}

// readLine reads from the shell's stdin until it reaches delim, or maxChars
// characters when it is not negative, or the end of the input. When exactly is
// set, delim is not looked for at all, as used by "read -N".
//
// Note that the returned line still holds the backslashes which escape another
// character, as whether they are dropped depends on the caller.
// deadlineReader is what a source has to be for `read -t` to bound it,
// and for the context to be able to interrupt it. A pipe and a terminal
// both qualify; a regular file does not, and does not need to, since it
// never blocks.
type deadlineReader interface {
	io.Reader
	SetReadDeadline(time.Time) error
}

// isRegularFile reports whether f is a plain file, which never blocks a
// read and so needs neither a deadline nor a poll.
func isRegularFile(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode().IsRegular()
}

// readLine reads one line, or one -n/-N-bounded run, from src.
//
// It reports whether the read timed out separately from the error,
// because a timeout is not a failure to the caller: bash assigns whatever
// it managed to read — `{ printf par; sleep 2; } | read -t 1 x` leaves x
// as "par" — and reports the timeout only through a status above 128
// (#267).
func (r *Runner) readLine(ctx context.Context, src io.Reader, raw bool, delim byte, maxChars int, exactly bool, timeout time.Duration) (line []byte, timedOut bool, _ error) {
	if src == nil {
		return nil, false, errors.New("interp: can't read, there's no stdin")
	}
	if maxChars == 0 {
		return nil, false, nil
	}

	esc := false
	// chars counts the characters that the line will hold once the escaping
	// backslashes are dropped, which is what -n and -N count. Characters,
	// not bytes (#377): а is one toward -n 5. pending tracks a multibyte
	// sequence in flight — its lead byte counts when the last
	// continuation byte arrives, and a stray continuation byte counts
	// alone, which is how bash's mbrtowc failure path treats it.
	chars := 0
	pending := 0
	countByte := func(b byte) {
		switch {
		case b < 0x80:
			chars++
			pending = 0
		case b >= 0xf8: // not a legal UTF-8 lead or continuation
			chars++
			pending = 0
		case b >= 0xf0:
			pending = 3
		case b >= 0xe0:
			pending = 2
		case b >= 0xc0:
			pending = 1
		default: // continuation byte
			if pending > 0 {
				pending--
				if pending == 0 {
					chars++
				}
			} else {
				chars++
			}
		}
	}

	// The deadline serves two callers at once: the context, which sets it to
	// now to interrupt a blocked read, and -t, which sets it ahead. Whichever
	// fires, the read returns os.ErrDeadlineExceeded and which one it was is
	// decided by asking the context afterwards.
	//
	// Not every blocking file takes a deadline: the runtime refuses one on
	// anything it cannot add to its poller, and a FIFO opened read-write —
	// `exec 9<> pipe`, the shape scripts use precisely to keep a FIFO from
	// blocking on open — is in that set. Treating the refusal as "a regular
	// file, returns immediately" left `read -u 9 -t 1` blocked until killed
	// (#348), so a refused deadline on something other than a regular file
	// falls back to poll(2) before each byte instead.
	var poll *os.File
	var pollDeadline time.Time
	if dr, ok := src.(deadlineReader); ok && dr.SetReadDeadline(time.Time{}) == nil {
		if timeout > 0 {
			dr.SetReadDeadline(time.Now().Add(timeout))
		}
		stopc := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			dr.SetReadDeadline(time.Now())
			close(stopc)
		})
		defer func() {
			if !stop() {
				// The AfterFunc was started.
				// Wait for it to complete before clearing the deadline.
				<-stopc
			}
			dr.SetReadDeadline(time.Time{})
		}()
	} else if f, ok := src.(*os.File); ok && !isRegularFile(f) {
		poll = f
		if timeout > 0 {
			pollDeadline = time.Now().Add(timeout)
		}
	}
	for {
		if poll != nil {
			pollTimedOut, err := waitReadable(ctx, poll, pollDeadline)
			if err != nil {
				return line, false, err
			}
			if pollTimedOut {
				return line, true, nil
			}
		}
		var buf [1]byte
		n, err := src.Read(buf[:])
		if n > 0 {
			b := buf[0]
			switch {
			case !raw && b == '\\':
				line = append(line, b)
				esc = !esc
				if !esc {
					// A second backslash, so the pair is one character.
					chars++
					pending = 0
				}
			case !raw && !exactly && b == delim && esc && delim == '\n':
				// line continuation; drop the trailing backslash
				line = line[:len(line)-1]
				esc = false
			case !exactly && b == delim && !esc:
				return line, false, nil
			default:
				// Note that an escaped delimiter lands here, so it becomes a
				// literal character rather than ending the line.
				line = append(line, b)
				esc = false
				countByte(b)
			}
			if maxChars >= 0 && chars >= maxChars {
				return line, false, nil
			}
		}
		if err != nil {
			// A deadline fired. It was -t unless the context is what
			// cancelled, in which case this is an interrupted command and
			// not a timeout the script asked for.
			if timeout > 0 && errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil {
				return line, true, nil
			}
			return line, false, err
		}
	}
}

// cdPathLookup searches CDPATH for a relative operand, which is how a
// script cds to a directory by name from anywhere (#391). An absolute
// or explicitly-relative path never searches, and a miss falls back to
// the operand as written.
func (r *Runner) cdPathLookup(ctx context.Context, path string) (string, bool) {
	if path == "" || filepath.IsAbs(path) ||
		strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") ||
		path == "." || path == ".." {
		return "", false
	}
	cdpath := r.envGet("CDPATH")
	if cdpath == "" {
		return "", false
	}
	for _, dir := range strings.Split(cdpath, ":") {
		if dir == "" {
			dir = "."
		}
		cand := filepath.Join(dir, path)
		if info, err := r.stat(ctx, r.absPath(cand)); err == nil && info.IsDir() {
			return cand, true
		}
	}
	return "", false
}

func (r *Runner) changeDir(ctx context.Context, cmd, path string) uint8 {
	if path == "" {
		r.errf("%s: empty directory path\n", cmd)
		return 1
	}
	apath := r.absPath(path)
	info, err := r.stat(ctx, apath)
	if err != nil || !info.IsDir() {
		r.errf("%s: no such file or directory: %q\n", cmd, path)
		return 1
	}
	if r.access(ctx, apath, AccessExec) != nil {
		r.errf("%s: permission denied: %q\n", cmd, path)
		return 1
	}
	r.Dir = apath
	r.setVarString("OLDPWD", r.envGet("PWD"))
	r.setVarString("PWD", apath)
	// Entry 0 of the directory stack *is* the current directory in
	// bash, so every chdir moves it (#390): leaving it frozen at the
	// shell's startup directory made popd return to the wrong place.
	r.dirStackSync()
	return 0
}

func absPath(dir, path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path) // TODO: this clean is likely unnecessary
}

func (r *Runner) absPath(path string) string {
	return absPath(r.Dir, path)
}

// flagParser is used to parse builtin flags.
//
// It's similar to the getopts implementation, but with some key differences.
// First, the API is designed for Go loops, making it easier to use directly.
// Second, it doesn't require the awkward ":ab" syntax that getopts uses.
// Third, it supports "-a" flags as well as "+a".
type flagParser struct {
	current   string
	remaining []string
}

func (p *flagParser) more() bool {
	if p.current != "" {
		// We're still parsing part of "-ab".
		return true
	}
	if len(p.remaining) == 0 {
		// Nothing left.
		p.remaining = nil
		return false
	}
	arg := p.remaining[0]
	if arg == "--" {
		// We explicitly stop parsing flags.
		p.remaining = p.remaining[1:]
		return false
	}
	if len(arg) == 0 || (arg[0] != '-' && arg[0] != '+') {
		// The next argument is not a flag.
		return false
	}
	// More flags to come.
	return true
}

func (p *flagParser) flag() string {
	arg := p.current
	if arg == "" {
		arg = p.remaining[0]
		p.remaining = p.remaining[1:]
	} else {
		p.current = ""
	}
	if len(arg) > 2 {
		// We have "-ab", so return "-a" and keep "-b".
		p.current = arg[:1] + arg[2:]
		arg = arg[:2]
	}
	return arg
}

func (p *flagParser) value() string {
	if len(p.remaining) == 0 {
		return ""
	}
	arg := p.remaining[0]
	p.remaining = p.remaining[1:]
	return arg
}

func (p *flagParser) args() []string { return p.remaining }

type getopts struct {
	argidx  int
	runeidx int
}

func (g *getopts) next(optstr string, args []string) (opt rune, optarg string, done bool) {
	if len(args) == 0 || g.argidx >= len(args) {
		return '?', "", true
	}
	arg := []rune(args[g.argidx])
	if len(arg) < 2 || arg[0] != '-' || arg[1] == '-' {
		return '?', "", true
	}

	opts := arg[1:]
	opt = opts[g.runeidx]

	i := strings.IndexRune(optstr, opt)
	if i >= 0 && i+1 < len(optstr) && optstr[i+1] == ':' {
		// the option requires an argument
		if g.runeidx+1 < len(opts) {
			// attached to the option in the same word, like -bval
			optarg = string(opts[g.runeidx+1:])
		} else if g.argidx+1 < len(args) {
			// the word that follows
			optarg = args[g.argidx+1]
			g.argidx++
		} else {
			// missing argument
			g.argidx++
			g.runeidx = 0
			return ':', string(opt), false
		}
		g.argidx++
		g.runeidx = 0
		return opt, optarg, false
	}

	if g.runeidx+1 < len(opts) {
		g.runeidx++
	} else {
		g.argidx++
		g.runeidx = 0
	}
	if i < 0 {
		// invalid option
		return '?', string(opt), false
	}
	return opt, "", false
}

// optStatusText returns a shell option's status text display
func (r *Runner) optStatusText(status bool) string {
	if status {
		return "on"
	}
	return "off"
}
