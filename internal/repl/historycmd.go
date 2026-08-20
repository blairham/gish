package repl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// history (#277): the script-facing half of the history engine koi
// already has.
//
// This is the shape #277 named as the cheap win. koi has a history store
// with live cross-session reload (#40), a completion engine, and the
// expansion `!!` needs — and none of it was reachable from a script,
// which answered `history: unsupported builtin` and exit 2 where bash
// answers silently and exit 0. The engines existed; the surface did not.
//
// It goes through the CallHandler rather than the ExecHandler for the
// reason printf does: the interpreter already claims the name, so a
// builtin it recognizes never reaches the exec seam.
//
// # What the list is
//
// bash keeps one in-memory list per shell. koi's store is *shared across
// sessions* (#40), so the two are not the same thing, and the difference
// has to be picked rather than papered over:
//
//   - Until something mutates it, `history` reports the store's recent
//     entries — which is what makes it agree with `fc -l` and with what
//     the user can actually see with the up-arrow.
//   - The first mutation (-c, -d, -s) snapshots that into a session list
//     and everything afterwards works on the snapshot. Otherwise `-c`
//     would be a no-op that looked like it worked, and `-d` would delete
//     from a file shared with every other running koi.
//
// So a script's mutations are session-local and never touch the store.
// That is the honest reading of "clear the history list" for a shell
// whose history is not a per-process list.
//
// # The file forms, split where the honesty line falls
//
// `-r file` and `-w file` are stateless — read a plain text file into
// the list, write the list out one command per line — so koi implements
// them completely and they do exactly what a script asking for them
// means. The first instinct was to refuse all four; that was wrong, and
// refusing something koi can do is not the same virtue as refusing
// something it cannot.
//
// `-a` and `-n` are the incremental pair: "append the lines *new since*
// the last write", "read the lines *not yet read*". Both need a
// per-file read position over a history that is one process's list. koi's
// is a store shared live across sessions (#40), where "new since" has no
// single answer, so those two report what they do not do — the same call
// this file's neighbor `fc` makes about its editing forms.
//
// With no operand, all of them need $HISTFILE. koi has no default for
// it: bash falls back to ~/.bash_history, and silently writing a file
// koi never reads is the no-op-that-reports-success this whole issue is
// about.
const historyUsage = `history: usage: history [-c] [-d offset] [n] or history -anrw [filename] or history -ps arg [arg...]`

// historyWindow bounds the store read, matching fc's own bound so the
// two agree about what "recent" means.
const historyWindow = fcCount

var (
	histMu sync.Mutex
	// histList is the session's own list, valid only once histMutated is
	// set. Guarded because koi's background jobs are goroutines, so
	// `history -s x &` really can race with a `history` on the main line.
	histList    []string
	histMutated bool
	// histBase is how many entries HISTSIZE has trimmed off the front.
	// bash's listing numbers keep advancing past a trim — after
	// HISTSIZE=2 drops two entries the next listing starts at 3 — while
	// `history -c` resets the numbering to 1 (both measured).
	histBase int
	// histAmbientLast marks that the newest list entry is the ambient
	// record (#277) of the line currently executing. `history -s` and
	// `history -p` replace that line rather than following it — bash
	// deletes the just-recorded `history -s …` entry before appending
	// the substitute (measured: the -s invocations never appear in the
	// listing, only what they stored).
	histAmbientLast bool
)

// historyAmbientLast reports whether the newest entry is the ambient
// record of the currently running line. `fc` reads this to leave its own
// line out of its view — bash's fc never lists itself, while a *later*
// command still sees the fc line in history, so the exclusion is a view
// and not a deletion (both measured).
func historyAmbientLast() bool {
	histMu.Lock()
	defer histMu.Unlock()
	return histAmbientLast
}

// historyPopSelf removes the ambient record of the currently running
// line, if that is what the newest entry is — the first half of
// `history -s` and `history -p`. No effect when recording is off or the
// line itself was filtered out, which is also when bash has nothing to
// delete.
func historyPopSelf() {
	histMu.Lock()
	defer histMu.Unlock()
	if histAmbientLast && histMutated && len(histList) > 0 {
		histList = histList[:len(histList)-1]
	}
	histAmbientLast = false
}

// historyBase returns the numbering offset trims have accumulated.
func historyBase() int {
	histMu.Lock()
	defer histMu.Unlock()
	return histBase
}

// historyEntries returns the list `history` reports, oldest first.
func historyEntries() []string {
	histMu.Lock()
	defer histMu.Unlock()
	return historyEntriesLocked()
}

func historyEntriesLocked() []string {
	if histMutated {
		return histList
	}
	if historyStore == nil {
		return nil
	}
	entries := historyStore.RecentEntries(historyWindow)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Command)
	}
	return out
}

// historyMutate takes the session's own copy, once.
func historyMutate(fn func(list []string) []string) {
	histMu.Lock()
	defer histMu.Unlock()
	if !histMutated {
		histList = historyEntriesLocked()
		histMutated = true
	}
	histList = fn(histList)
	// Whatever the newest entry is now, it is not the ambient record of
	// the running line anymore.
	histAmbientLast = false
}

func historyCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "history" {
			return next(ctx, args)
		}
		return runHistory(interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runHistory(hc interp.HandlerContext, args []string) []string {
	// bash parses these as a set rather than in order, and rejects more
	// than one of -anrw before doing anything.
	var fileFlags []string
	clear, del, appendArg, expand := false, "", false, false

	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		flag := args[0]
		args = args[1:]
		switch flag {
		case "--":
			goto operands
		case "-c":
			clear = true
		case "-d":
			if len(args) == 0 {
				fmt.Fprintf(hc.Stderr, "history: -d: option requires an argument\n%s\n", historyUsage)
				return historyStatus(2)
			}
			del, args = args[0], args[1:]
		case "-s":
			appendArg = true
		case "-p":
			expand = true
		case "-a", "-n", "-r", "-w":
			fileFlags = append(fileFlags, flag)
		default:
			fmt.Fprintf(hc.Stderr, "history: %s: invalid option\n%s\n", flag, historyUsage)
			return historyStatus(2)
		}
	}

operands:
	if len(fileFlags) > 1 {
		fmt.Fprintln(hc.Stderr, "history: cannot use more than one of -anrw")
		return historyStatus(1)
	}
	if len(fileFlags) == 1 {
		return historyFile(hc, fileFlags[0], args)
	}

	switch {
	case expand:
		return historyExpand(hc, args)
	case appendArg:
		if len(args) == 0 {
			return historyStatus(0)
		}
		// `history -s` *replaces* its own line: bash deletes the entry
		// recording the `history -s …` command itself, then runs the
		// substitute through the same HISTCONTROL/HISTIGNORE gate as
		// ambient recording — `history -s ' spaced'` under ignoreboth
		// deletes and records nothing (both measured).
		historyPopSelf()
		entry := strings.Join(args, " ")
		historyAppendFiltered(entry, false, func(name string) string {
			return sessionVarOf(sessionRunner(), name)
		})
		return historyStatus(0)
	case del != "":
		return historyDelete(hc, del)
	case clear:
		historyMutate(func([]string) []string { return nil })
		// Clearing restarts the numbering at 1, unlike a HISTSIZE trim
		// (measured against bash 5.3).
		histMu.Lock()
		histBase = 0
		histMu.Unlock()
		return historyStatus(0)
	}
	return historyList(hc, args)
}

// historyList prints the list, numbered from 1 as bash does. An optional
// count shows only the newest n, keeping their real positions.
func historyList(hc interp.HandlerContext, args []string) []string {
	entries := historyEntries()
	base := historyBase()
	first := 0
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 0 {
			// No usage line here, deliberately: bash prints it for a bad
			// *option* and not for a bad operand, and the two were
			// measured rather than assumed.
			fmt.Fprintf(hc.Stderr, "history: %s: numeric argument required\n", args[0])
			return historyStatus(2)
		}
		if n < len(entries) {
			first = len(entries) - n
		}
	}
	for i := first; i < len(entries); i++ {
		fmt.Fprintf(hc.Stdout, "%5d  %s\n", i+1+base, entries[i])
	}
	return historyStatus(0)
}

// historyDelete removes one entry by position. The list is renumbered
// afterwards, which is what bash reports too.
func historyDelete(hc interp.HandlerContext, spec string) []string {
	n, err := strconv.Atoi(spec)
	if err != nil {
		// bash's wording for -d differs from the one above: "invalid
		// number", not "numeric argument required", and again no usage.
		fmt.Fprintf(hc.Stderr, "history: %s: invalid number\n", spec)
		return historyStatus(1)
	}
	bad := false
	base := historyBase()
	historyMutate(func(list []string) []string {
		i := n
		if i < 0 { // bash counts back from the newest
			i = len(list) + 1 + i
		} else {
			// Positive offsets are the numbers the listing shows, which
			// run ahead of list positions once HISTSIZE has trimmed.
			i -= base
		}
		if i < 1 || i > len(list) {
			bad = true
			return list
		}
		return append(list[:i-1:i-1], list[i:]...)
	})
	if bad {
		fmt.Fprintf(hc.Stderr, "history: %s: history position out of range\n", spec)
		return historyStatus(1)
	}
	return historyStatus(0)
}

// historyExpand is `history -p`: expand the arguments and print the
// result without running or recording anything.
//
// This is the one that connects the builtin to the engine koi already
// had — the same expander the interactive line goes through (#96), so
// `history -p '!!'` and typing `!!` cannot disagree.
func historyExpand(hc interp.HandlerContext, args []string) []string {
	// `history -p` removes its own line first (measured), which is also
	// what makes its `!!` mean the previous command rather than itself.
	historyPopSelf()
	entries := historyEntries()
	match := func(prefix string, n int) (string, bool) {
		// Newest first, skipping n matches, which is the contract
		// history.Store.Match has and expandHistory expects.
		seen := 0
		for i := len(entries) - 1; i >= 0; i-- {
			if !strings.HasPrefix(entries[i], prefix) {
				continue
			}
			if seen == n {
				return entries[i], true
			}
			seen++
		}
		return "", false
	}
	status := 0
	for _, arg := range args {
		expanded, _, err := expandHistory(arg, match)
		if err != nil {
			// bash's wording is "history expansion failed"; the event it
			// could not find is the useful half and it keeps that.
			fmt.Fprintf(hc.Stderr, "history: %v\n", err)
			status = 1
			continue
		}
		fmt.Fprintln(hc.Stdout, expanded)
	}
	return historyStatus(status)
}

// historyFile serves -r and -w, and reports what -a and -n do not do.
func historyFile(hc interp.HandlerContext, flag string, args []string) []string {
	switch flag {
	case "-a", "-n":
		// See the package comment: both mean "the lines new since last
		// time", which needs a read position over a per-process list.
		fmt.Fprintf(hc.Stderr,
			"history: %s: not implemented; it means \"the lines new since last time\", "+
				"and koi's history is a store shared live across sessions (#40)\n", flag)
		return historyStatus(1)
	}

	path := ""
	if len(args) > 0 {
		path = args[0]
	} else if path = os.Getenv("HISTFILE"); path == "" {
		// bash falls back to ~/.bash_history. koi does not have one, and
		// writing a file it never reads back would be the no-op that
		// reports success this builtin exists to stop.
		fmt.Fprintf(hc.Stderr, "history: %s: no file given and HISTFILE is unset\n", flag)
		return historyStatus(1)
	}

	// Relative to the *shell's* directory, not the Go process's. They
	// are not the same after a `cd`, and resolving against the wrong one
	// is a bug that only shows up in a script that changed directory —
	// which is most of them.
	if !filepath.IsAbs(path) {
		path = filepath.Join(hc.Dir, path)
	}

	if flag == "-r" {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "history: %s: %v\n", path, err)
			return historyStatus(1)
		}
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil // an empty file adds nothing, rather than one empty entry
		}
		historyMutate(func(list []string) []string { return append(list, lines...) })
		return historyStatus(0)
	}

	var b strings.Builder
	for _, cmd := range historyEntries() {
		b.WriteString(cmd)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		fmt.Fprintf(hc.Stderr, "history: %s: %v\n", path, err)
		return historyStatus(1)
	}
	return historyStatus(0)
}

// historyStatus answers with an arbitrary exit status, the way printf's
// handler does: a CallHandler can only rewrite the call, and `true` and
// `false` cover 0 and 1 but not bash's 2 for a usage error.
func historyStatus(status int) []string {
	switch status {
	case 0:
		return []string{"true"}
	case 1:
		return []string{"false"}
	}
	return []string{"eval", fmt.Sprintf("(exit %d)", status)}
}
