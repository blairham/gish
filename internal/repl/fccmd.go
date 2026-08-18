package repl

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/koi-shell/internal/history"
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

	store := historyStore
	if store == nil {
		fmt.Fprintln(hc.Stderr, "fc: no history in this session")
		return []string{"false"}
	}
	entries := store.RecentEntries(fcCount)
	if len(entries) == 0 {
		fmt.Fprintln(hc.Stderr, "fc: no history yet")
		return []string{"false"}
	}

	first, last, err := fcRange(args, len(entries))
	if err != nil {
		fmt.Fprintf(hc.Stderr, "fc: %v\n%s\n", err, fcUsage)
		return []string{"false"}
	}

	idx := make([]int, 0, last-first+1)
	for i := first; i <= last; i++ {
		idx = append(idx, i)
	}
	if reverse {
		for i, j := 0, len(idx)-1; i < j; i, j = i+1, j-1 {
			idx[i], idx[j] = idx[j], idx[i]
		}
	}
	for _, i := range idx {
		writeFCLine(hc, i, entries[i-1], numbered)
	}
	return []string{"true"}
}

// writeFCLine prints one entry, numbered the way bash does.
func writeFCLine(hc interp.HandlerContext, n int, e history.Entry, numbered bool) {
	if numbered {
		fmt.Fprintf(hc.Stdout, "%5d  %s\n", n, e.Command)
		return
	}
	fmt.Fprintf(hc.Stdout, "\t%s\n", e.Command)
}

// fcRange resolves the first/last operands against a history of n
// entries, numbered 1..n.
//
// Negative numbers count back from the newest, which is how people
// reach for "the last five" without knowing where they are.
func fcRange(args []string, n int) (first, last int, err error) {
	switch len(args) {
	case 0:
		first, last = n-fcDefaultList+1, n
	case 1:
		if first, err = fcIndex(args[0], n); err != nil {
			return 0, 0, err
		}
		last = n
	case 2:
		if first, err = fcIndex(args[0], n); err != nil {
			return 0, 0, err
		}
		if last, err = fcIndex(args[1], n); err != nil {
			return 0, 0, err
		}
	default:
		return 0, 0, fmt.Errorf("too many arguments")
	}
	if first < 1 {
		first = 1
	}
	if last > n {
		last = n
	}
	if first > last {
		// A reversed range is a request for newest-first in bash; here it
		// would print nothing, which reads as a broken command.
		first, last = last, first
	}
	return first, last, nil
}

func fcIndex(s string, n int) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: history positions must be numbers", s)
	}
	if v < 0 {
		return n + v + 1, nil
	}
	return v, nil
}
