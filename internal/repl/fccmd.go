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
// fc, `fc -e` and `fc -s`) re-execute a command, which means reaching
// into the REPL's own run loop rather than adding a builtin; they report
// what they do not do instead of half-working, because a history editor
// that silently ran the wrong entry would be worse than one that is
// absent.
//
// Numbering counts from the oldest entry held, so the numbers `fc -l`
// prints are the ones its own range arguments accept. bash numbers
// across the whole session history; koi's store is shared across
// sessions (#40), so the numbers are positions in the recent window
// rather than a global sequence — stated by `help fc` rather than left
// for someone to infer from a mismatch, and deliberately *not* by the
// usage line (see fcUsage).

// fcCount bounds the window fc works over, matching the picker's own
// bound so the two agree about what "recent" means.
const fcCount = 5000

// fcDefaultList is how many entries a bare `fc -l` shows, following
// bash.
const fcDefaultList = 16

// fcUsage is bash's own usage line, byte for byte (#611).
//
// It used to be eleven lines of prose explaining koi's history positions
// and why the editing forms are absent — honest, and in the wrong place,
// the same mistake as #574's `("on" not supported)` annotation. A usage
// line is *data*: it is the most-read output of a failing builtin and a
// caller may match it, so eleven lines where bash prints one is a
// divergence on the one form of the command everybody sees. The
// explanation moved to `help fc`, which is where someone goes to be
// explained to; the refusal message below is where the honesty about
// what fc does not do belongs.
//
// It advertises `-e` and `-s`, which koi does not implement, because it
// is bash's line rather than a description of koi — and neither answers
// "invalid option": both are recognized and refused by name.
const fcUsage = "fc: usage: fc [-e ename] [-lnr] [first] [last] or fc -s [pat=rep] [command]"

// fcNotes are the koi-specific facts about fc, printed by `help fc`.
// They are what the usage line used to carry.
var fcNotes = []string{
	"Numbers are positions in the recent history window, not a",
	"session-global sequence: koi's history is shared across sessions",
	"(#40), so `fc -l` numbers what it can see rather than a count that",
	"survives a reboot.",
	"",
	"Only the listing half is implemented. The editing forms (fc with no",
	"-l, fc -e, and fc -s) re-execute a command, which belongs to the",
	"shell's run loop rather than to a builtin; they say so rather than",
	"half-working, because a history editor that silently ran the wrong",
	"entry would be worse than one that is absent.",
}

// fcNotImplemented is what every editing form answers with. One string,
// so the four ways of asking for one give the same answer.
const fcNotImplemented = "fc: only the listing form is implemented; use `fc -l`"

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
		for i, r := range flag[1:] {
			switch r {
			case 'l':
				list = true
			case 'n':
				numbered = false
			case 'r':
				reverse = true
			case 'e':
				// bash's -e takes an editor name, and a missing one is a
				// usage error rather than an invalid option — measured
				// against bash 5.3 rather than derived from -s, which
				// takes no argument at all.
				if flag[2+i:] == "" && len(args) < 2 {
					hc.Errf("fc: -e: option requires an argument\n")
					hc.RawErrf("%s\n", fcUsage)
					return []string{"false"}
				}
				// An editor is an editing form, so it gets the refusal
				// rather than "invalid option": the option exists, the
				// behavior behind it does not.
				hc.Errf("%s\n", fcNotImplemented)
				return []string{"false"}
			case 's':
				// Recognized so it gets the real explanation rather than
				// "invalid option", which would suggest a typo.
				hc.Errf("%s\n", fcNotImplemented)
				return []string{"false"}
			default:
				// The status stays 1 where bash answers 2: a builtin's
				// exit status for a bad option is #577's subject, and
				// this change is about what is printed.
				hc.Errf("fc: -%c: invalid option\n", r)
				hc.RawErrf("%s\n", fcUsage)
				return []string{"false"}
			}
		}
		args = args[1:]
	}

	if !list {
		// Being explicit beats "unsupported builtin": the reader learns
		// which half exists and what to type instead.
		hc.Errf("%s\n", fcNotImplemented)
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
		hc.Errf("fc: %v\n", err)
		hc.RawErrf("%s\n", fcUsage)
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
