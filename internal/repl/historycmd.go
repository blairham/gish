package repl

import (
	"context"
	"fmt"
	"os"
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
// the list, write the list out one command per line. `-a` and `-n` are
// the incremental pair, and they refused until a script session had a
// list of its own: both mean "the lines new since last time", which
// needs a position over a per-process history rather than over a store
// shared live across sessions (#40). Ambient recording (#277) supplied
// that list, so all four now work on the script paths; the positions and
// the $HISTFILE preload live in histfile.go (#432).
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
	// histAppendPos is where `history -a` starts writing, in the same
	// absolute coordinate as the listing numbers (histBase + index)
	// rather than as an index into the current slice. That is what makes
	// it survive an HISTSIZE trim: the entries drop off the front, the
	// base advances, and the position still names the same command —
	// measured, since a stifled list appends exactly the two entries
	// recorded since the last write, not whatever the raw index lands on.
	histAppendPos int
	// histFileLines is how many lines of the history file this session
	// has accounted for: what the preload read, what `-a` wrote, and
	// what `-n` has already consumed. bash keeps one counter for all
	// three, which is why `-a` and `-n` interlock — appending two lines
	// leaves `-n` with nothing to read, and a `-r` of *another* file
	// resets it to that file's length (all measured).
	histFileLines int
	// histPreloaded records that the load-on-enable already happened.
	// bash loads $HISTFILE once per shell, at the moment `set -o
	// history` turns recording on: re-enabling does not reload, and a
	// HISTFILE assigned afterwards is never read (measured).
	histPreloaded bool
	// histAmbientSession marks a session that records ambiently (#277) —
	// the script paths, which are the ones with a per-process list for
	// the file forms to have positions over. An interactive session's
	// history is the shared store (#40) and must not be replaced by a
	// file's contents when an rc happens to say `set -o history`.
	histAmbientSession bool
)

// historyAmbientSession marks this session as one that records ambiently.
func historyAmbientSession() {
	histMu.Lock()
	defer histMu.Unlock()
	histAmbientSession = true
}

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
	clear, appendArg, expand := false, false, false
	// del is a pointer rather than a string because `history -d ''` is a
	// real request — bash answers it `: invalid number` — and an empty
	// string is indistinguishable from the option not being given.
	var del *string

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
				hc.Errf("history: -d: option requires an argument\n")
				hc.RawErrf("%s\n", historyUsage)
				return historyStatus(2)
			}
			del, args = &args[0], args[1:]
		case "-s":
			appendArg = true
		case "-p":
			expand = true
		case "-a", "-n", "-r", "-w":
			fileFlags = append(fileFlags, flag)
		default:
			hc.Errf("history: %s: invalid option\n", flag)
			hc.RawErrf("%s\n", historyUsage)
			return historyStatus(2)
		}
	}

operands:
	if len(fileFlags) > 1 {
		hc.Errf("history: cannot use more than one of -anrw\n")
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
	case del != nil:
		return historyDelete(hc, *del)
	case clear:
		historyMutate(func([]string) []string { return nil })
		// Clearing restarts the numbering at 1, unlike a HISTSIZE trim
		// (measured against bash 5.3).
		histMu.Lock()
		histBase = 0
		// The append position goes with it: nothing survives the clear
		// for `-a` to have already written (measured — a `-c` then one
		// command appends exactly that command).
		histAppendPos = 0
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
			hc.Errf("history: %s: numeric argument required\n", args[0])
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

// historyDelete removes entries by position. The list is renumbered
// afterwards, which is what bash reports too.
//
// bash takes three shapes here and koi took one (#710): a single offset,
// a `first-last` range, and either end of either counting back from the
// newest — so `history -d 2-4`, `history -d -1` and `history -d 6--1` are
// all ordinary, and reading the whole operand as one number made every
// one of them `invalid number`. history3.sub is these end to end.
//
// The two diagnostics are not interchangeable, which is the half a caller
// can act on: `invalid number` means the operand is not a number at all,
// and `history position out of range` means it is one the list does not
// have — so `history -d 5-0xaf` is the *second* of those, because bash
// reads numbers in base ten and `0xaf` stops at the `x` rather than
// failing to be a number (measured; the issue predicted the other way).
func historyDelete(hc interp.HandlerContext, spec string) []string {
	// A range is a `-` after any leading sign, which is how `-2--1` splits
	// into `-2` and `-1` rather than at its first character.
	skip := 0
	if strings.HasPrefix(spec, "-") {
		skip = 1
	}
	if at := strings.IndexByte(spec[skip:], '-'); at >= 0 {
		return historyDeleteRange(hc, spec[:skip+at], spec[skip+at+1:], spec)
	}

	n, ok := histNumber(spec)
	if !ok {
		// bash's wording for -d differs from the one above: "invalid
		// number", not "numeric argument required", and again no usage.
		hc.Errf("history: %s: invalid number\n", spec)
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
		hc.Errf("history: %s: history position out of range\n", spec)
		return historyStatus(1)
	}
	return historyStatus(0)
}

// historyDeleteRange is `history -d first-last`. whole is the operand as
// written, which is what bash names when either half is not a number at
// all — it puts the `-` back before complaining, so the diagnostic is
// about the range rather than about the piece that failed.
func historyDeleteRange(hc interp.HandlerContext, firstArg, lastArg, whole string) []string {
	first, firstOK := histNumber(firstArg)
	last, lastOK := histNumber(lastArg)
	if !firstOK || !lastOK {
		hc.Errf("history: %s: history position out of range\n", whole)
		return historyStatus(1)
	}
	// Which half was out of range decides which half is named, so the two
	// are reported separately rather than after both are resolved.
	badArg := ""
	// end < start deletes nothing and answers 1 with **no message at all**
	// — readline's remove_history_range refuses the pair and the builtin
	// only passes its result on (measured: `history -d 3-1` is a silent
	// failure).
	empty := false
	base := historyBase()
	historyMutate(func(list []string) []string {
		// Both halves are 0-based positions here. A leading `-` on the half
		// itself is what makes it count back, so a negative reached any
		// other way — leading whitespace, say — is simply out of range.
		lo := histRangeEnd(first, firstArg, len(list), base)
		if lo < 0 || lo >= len(list) {
			badArg = firstArg
			return list
		}
		hi := histRangeEnd(last, lastArg, len(list), base)
		if hi < 0 || hi >= len(list) {
			badArg = lastArg
			return list
		}
		if hi < lo {
			empty = true
			return list
		}
		return append(list[:lo:lo], list[hi+1:]...)
	})
	switch {
	case badArg != "":
		hc.Errf("history: %s: history position out of range\n", badArg)
		return historyStatus(1)
	case empty:
		return historyStatus(1)
	}
	return historyStatus(0)
}

// histRangeEnd resolves one half of a range to a 0-based position. Zero
// is deliberately left alone rather than offset by the base, which is
// bash's own asymmetry: `history -d 0` is out of range while
// `history -d 0-1` starts at the oldest entry.
func histRangeEnd(n int, arg string, length, base int) int {
	switch {
	case strings.HasPrefix(arg, "-") && n < 0:
		return length + n
	case n > 0:
		return n - base - 1
	}
	return n
}

// histNumber is bash's valid_number: base ten, an optional sign,
// surrounding whitespace allowed, and the whole string consumed. It is
// not `strconv.Atoi` because the differences are both load-bearing —
// `history -d ' 2 '` deletes entry 2 and `history -d 5-0xaf` is an
// out-of-range range rather than a bad number, since `0xaf` reads as far
// as `0` and then fails on the `x`.
func histNumber(s string) (int, bool) {
	s = strings.Trim(s, " \t")
	if s == "" {
		return 0, false
	}
	digits := s
	neg := false
	switch digits[0] {
	case '-':
		neg = true
		digits = digits[1:]
	case '+':
		digits = digits[1:]
	}
	if digits == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
		n = n*10 + int(digits[i]-'0')
		if n > 1<<40 {
			// Far past any history a shell holds; kept from overflowing
			// rather than reported, since bash reads the whole number and
			// then finds it out of range.
			n = 1 << 40
		}
	}
	if neg {
		n = -n
	}
	return n, true
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
	// The same lookup a script's own lines are expanded against (#559),
	// so `history -p '!!'` and a bare `!!` cannot disagree — and the same
	// $histchars, since a script that moved the expansion character moved
	// it for `history -p` too (#695).
	src := sessionHistorySource()
	chars := sessionHistChars(sessionRunner())
	status := 0
	for _, arg := range args {
		expanded, _, err := expandHistory(arg, src, chars)
		if err != nil {
			// `history -p` has one wording for every failure and names
			// the *argument* rather than the designator inside it —
			// measured, and different from what the same failure prints
			// when the shell's own reader hits it, which is why this is
			// not the expander's message.
			hc.Errf("history: %s: history expansion failed\n", arg)
			status = 1
			continue
		}
		fmt.Fprintln(hc.Stdout, expanded)
	}
	return historyStatus(status)
}

// historyFile serves the four file forms over the session list (#432).
func historyFile(hc interp.HandlerContext, flag string, args []string) []string {
	runner := sessionRunner()
	path := ""
	switch {
	case len(args) > 0:
		path = args[0]
	default:
		// The live value, not os.Getenv: a script assigns HISTFILE as it
		// goes, and the process environment never sees that.
		path = sessionVarOf(runner, "HISTFILE")
		if path == "" {
			// bash falls back to ~/.bash_history. koi does not have one,
			// and writing a file it never reads back would be the
			// no-op that reports success this builtin exists to stop.
			return historyNoFile(hc, flag)
		}
	}

	switch flag {
	case "-a":
		if err := historyAppendNew(hc, path); err != nil {
			hc.Errf("history: %s: %v\n", path, err)
			return historyStatus(1)
		}
		return historyStatus(0)
	case "-n":
		if err := historyReadNew(hc, path, runner); err != nil {
			hc.Errf("history: %s: %v\n", path, err)
			return historyStatus(1)
		}
		return historyStatus(0)
	case "-r":
		lines, err := historyFileLines(hc, path)
		if err != nil {
			hc.Errf("history: %s: %v\n", path, err)
			return historyStatus(1)
		}
		historyReadAll(lines, runner)
		return historyStatus(0)
	}

	path = historyFilePath(hc, path)

	var b strings.Builder
	for _, cmd := range historyEntries() {
		b.WriteString(cmd)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		hc.Errf("history: %s: %v\n", path, err)
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
