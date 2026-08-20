package repl

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// fc (#60): the POSIX history builtin.
//
// Scoped deliberately to the listing half. `fc -l` is what people
// actually type — "what did I just run, with numbers I can refer to" —
// and it needs nothing but the history store. The editing forms (bare
// fc, and `fc -s`) re-execute a command, which means reaching into the
// REPL's own run loop rather than adding a builtin; they report what
// they do not do instead of half-working, because a history editor that
// silently ran the wrong entry would be worse than one that is absent.
//
// Numbering counts from the oldest entry held, so the numbers `fc -l`
// prints are the ones its own range arguments accept. bash numbers
// across the whole session history; koi's store is shared across
// sessions (#40), so the numbers are positions in the recent window
// rather than a global sequence — stated in the help rather than left
// for someone to infer from a mismatch.

// fcCount bounds the window fc works over, matching the picker's own
// bound so the two agree about what "recent" means.
const fcCount = 5000

// fcDefaultList is how many entries a bare `fc -l` shows, following
// bash.
const fcDefaultList = 16

const fcUsage = `usage: fc -l [-nr] [first] [last]

  fc -l              list the last 16 commands, numbered
  fc -l 5            list from entry 5 to the end
  fc -l 5 10         list entries 5 through 10
  fc -l -n           omit the numbers
  fc -l -r           newest first

Numbers are positions in the recent history window, not a session-global
sequence: koi's history is shared across sessions (#40).

Editing forms (fc without -l, and fc -s) are not implemented — they
re-execute a command, which belongs to the shell's run loop rather than
to a builtin.`

// fcCallHandler intercepts `fc`, which the interpreter claims but does
// not implement.
func fcCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "fc" {
			return next(ctx, args)
		}
		return runFC(interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runFC(hc interp.HandlerContext, args []string) []string {
	list, numbered, reverse := false, true, false
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		flag := args[0]
		if flag == "--" {
			args = args[1:]
			break
		}
		// `fc -l -2` asks for the last two, not for an option named 2.
		// bash reads a leading minus followed by digits as the *first*
		// operand counting back from the newest entry, which fcRange
		// below already understands -- the flag loop was eating it first
		// and answering "invalid option" (#306).
		if isNegativeNumber(flag) {
			break
		}
		// Flags cluster: -ln and -l -n mean the same thing.
		for _, r := range flag[1:] {
			switch r {
			case 'l':
				list = true
			case 'n':
				numbered = false
			case 'r':
				reverse = true
			case 's':
				// Recognized so it gets the real explanation rather than
				// "invalid option", which would suggest a typo.
				fmt.Fprintln(hc.Stderr, "fc: only the listing form is implemented; use `fc -l`")
				return []string{"false"}
			case 'h', '?':
				fmt.Fprintln(hc.Stdout, fcUsage)
				return []string{"true"}
			default:
				fmt.Fprintf(hc.Stderr, "fc: -%c: invalid option\n%s\n", r, fcUsage)
				return []string{"false"}
			}
		}
		args = args[1:]
	}

	if !list {
		// Being explicit beats "unsupported builtin": the reader learns
		// which half exists and what to type instead.
		fmt.Fprintln(hc.Stderr, "fc: only the listing form is implemented; use `fc -l`")
		return []string{"false"}
	}

	// The same list `history` reports, rather than the store underneath it
	// (#306). `history -s` writes into a session-local copy, so reading
	// the store directly meant `history` listing two commands while `fc
	// -l` said there was no history at all -- and under `koi -c`, where
	// there is no store, fc said that about every session.
	entries := historyEntries()
	// bash's fc never lists its own invocation, while a later command
	// still finds the fc line in history — so its just-recorded line is
	// left out of the view rather than deleted (#277, both measured).
	if historyAmbientLast() && len(entries) > 0 {
		entries = entries[:len(entries)-1]
	}
	if len(entries) == 0 {
		// bash prints nothing and succeeds. Nothing to list is not an
		// error: `fc -l` in a fresh shell is a reasonable thing for a
		// script to do, and answering it with status 1 ends one running
		// under `set -e`.
		return []string{"true"}
	}

	// Operands are the numbers the listings show, which run ahead of list
	// positions once HISTSIZE has trimmed entries off the front (#277).
	base := historyBase()
	first, last, err := fcRange(args, len(entries), base)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "fc: %v\n%s\n", err, fcUsage)
		return []string{"false"}
	}

	// first > last is a listing direction (see fcRange): walk the range
	// in the order the operands gave it, then let -r invert that.
	step := 1
	if first > last {
		step = -1
	}
	idx := make([]int, 0, (last-first)*step+1)
	for i := first; ; i += step {
		idx = append(idx, i)
		if i == last {
			break
		}
	}
	if reverse {
		for i, j := 0, len(idx)-1; i < j; i, j = i+1, j-1 {
			idx[i], idx[j] = idx[j], idx[i]
		}
	}
	for _, i := range idx {
		writeFCLine(hc, i+base, entries[i-1], numbered)
	}
	return []string{"true"}
}

// writeFCLine prints one entry the way bash's `fc -l` does: the number,
// a tab, a space, the command -- and with -n, the tab and space alone.
//
// Measured from bash 5.3 rather than borrowed from the builtin next door.
// This used to print `%5d  %s`, which is `history`'s format, so anything
// cutting a field out of `fc -l` got a different shape than bash gives it
// (#306).
func writeFCLine(hc interp.HandlerContext, n int, command string, numbered bool) {
	if numbered {
		fmt.Fprintf(hc.Stdout, "%d\t %s\n", n, command)
		return
	}
	fmt.Fprintf(hc.Stdout, "\t %s\n", command)
}

// fcRange resolves the first/last operands against a history of n
// entries, held as positions 1..n but displayed as base+1..base+n once
// HISTSIZE trims have advanced the numbering (#277).
//
// Negative numbers count back from the newest, which is how people
// reach for "the last five" without knowing where they are.
func fcRange(args []string, n, base int) (first, last int, err error) {
	switch len(args) {
	case 0:
		first, last = n-fcDefaultList+1, n
	case 1:
		if first, err = fcIndex(args[0], n, base); err != nil {
			return 0, 0, err
		}
		last = n
	case 2:
		if first, err = fcIndex(args[0], n, base); err != nil {
			return 0, 0, err
		}
		if last, err = fcIndex(args[1], n, base); err != nil {
			return 0, 0, err
		}
	default:
		return 0, 0, fmt.Errorf("too many arguments")
	}
	// bash's out-of-range rule, measured against 5.3 rather than assumed:
	// any spec outside [1, n] switches to the *whole* list, oldest first
	// — `fc -l 25` over twenty entries lists all twenty, not a default
	// window and not an error. The previous version clamped before it
	// swapped a reversed pair, so `fc -l 4` over two entries carried 4
	// into `last` after the ceiling check had already run — the caller
	// indexed entries[3] and panicked, and under the panic guard that
	// abandoned the rest of the input (#277: history.tests lost 368 of
	// its 410 lines to this one crash).
	if first < 1 || first > n || last < 1 || last > n {
		return 1, n, nil
	}
	// An in-range reversed pair keeps its order: `fc -l 3 1` prints
	// 3, 2, 1 in bash, so first > last is a listing direction, not a
	// mistake to normalize away.
	return first, last, nil
}

func fcIndex(s string, n, base int) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: history positions must be numbers", s)
	}
	if v < 0 {
		return n + v + 1, nil
	}
	if v == 0 {
		// `fc -l 0` lists the newest entry in bash — zero resolves to
		// the end of the list, not to an out-of-range position.
		return n, nil
	}
	return v - base, nil
}

// isNegativeNumber reports whether an argument is a negative count rather
// than a flag, which for fc is the difference between "the last two" and a
// usage error.
func isNegativeNumber(arg string) bool {
	digits, ok := strings.CutPrefix(arg, "-")
	if !ok || digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
