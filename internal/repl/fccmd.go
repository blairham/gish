package repl

import (
	"context"
	"fmt"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// fc (#60, #711): the POSIX history builtin.
//
// Two of the three forms are here. `fc -l` lists — "what did I just run,
// with numbers I can refer to" — and needs nothing but the history store.
// `fc -s` (and `fc -e -`, the same thing spelled the other way)
// re-executes, which is what `alias r='fc -s'` exists for.
//
// The re-execution is *not* done by the builtin, which was the original
// reason the whole family was absent: a command run from inside a
// CallHandler would be running outside the shell's own path, and the
// interpreter treats a CallHandler error as fatal. It is handed back as
// an `eval` rewrite instead — the seam `config` and `zi` already use — so
// the interpreter runs it, and `(exit 42); fc -s` answers 42. See
// fcExecute.
//
// The **editor** form — bare `fc`, and `fc -e ename` — is still refused,
// and now for a specific reason rather than a general one: bash echoes
// the edited file back as it reads it, which is `set -v`, and koi answers
// that option with "cannot turn verbose on: not implemented" (#734). So
// the form could not be matched even with the editor running, and it says
// what it
// does not do rather than half-working — a history editor that silently
// ran the wrong entry would be worse than one that is absent.
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
	"The listing form (fc -l) and the re-execute form (fc -s, fc -e -)",
	"are implemented. The editor form — fc with no -l, and fc -e ename —",
	"is not: it needs the shell to echo the edited file back as it reads",
	"it, which is `set -v`, and koi does not have that option yet. It",
	"says so rather than half-working, because a history editor that",
	"silently ran the wrong entry would be worse than one that is absent.",
}

// fcNotImplemented is what the bare editing form answers with — `fc`
// with no `-l`, `-s` or `-e -`, which means "open the range in $FCEDIT".
const fcNotImplemented = "fc: only the listing and re-execute forms are implemented; use `fc -l` or `fc -s`"

// fcNoEditor is what `fc -e ename` answers. It is a different message
// from the one above on purpose: the re-execute form now works, so the
// only thing left unimplemented is running an editor over the range, and
// a message still saying "only the listing form" would send the reader
// away from `fc -s`, which is what they wanted.
const fcNoEditor = "fc: the editor form is not implemented; use `fc -s` to re-execute or `fc -l` to list"

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
	list, numbered, reverse, execute := false, true, false, false
	ename := ""
	editorGiven := false
flags:
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
				// takes no argument at all. It ends the cluster: everything
				// after the letter is the name.
				switch rest := flag[2+i:]; {
				case rest != "":
					ename = rest
				case len(args) >= 2:
					ename, args = args[1], args[1:]
				default:
					hc.Errf("fc: -e: option requires an argument\n")
					hc.RawErrf("%s\n", fcUsage)
					return []string{"false"}
				}
				editorGiven = true
				args = args[1:]
				continue flags
			case 's':
				execute = true
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

	// `fc -e -` is `fc -s` spelled the other way: the editor named is the
	// one that does nothing, so the range is re-executed unedited.
	if ename == "-" {
		execute = true
	}
	if execute {
		return fcExecute(hc, args)
	}

	if !list {
		// The editor form. Its *specification* is still read here, because
		// bash reports a bad one before it ever reaches an editor — which
		// is why `fc -0` is `history specification out of range` rather
		// than the refusal below (history5.sub, measured).
		if st, bad := fcCheckEditSpec(hc, args); bad {
			return st
		}
		// Being explicit beats "unsupported builtin": the reader learns
		// which half exists and what to type instead.
		if editorGiven {
			hc.Errf("%s\n", fcNoEditor)
			return []string{"false"}
		}
		hc.Errf("%s\n", fcNotImplemented)
		return []string{"false"}
	}

	// The same list `history` reports, rather than the store underneath it
	// (#306). `history -s` writes into a session-local copy, so reading
	// the store directly meant `history` listing two commands while `fc
	// -l` said there was no history at all -- and under `koi -c`, where
	// there is no store, fc said that about every session.
	all := historyEntries()
	if len(all) == 0 {
		// bash prints nothing and succeeds. Nothing to list is not an
		// error: `fc -l` in a fresh shell is a reasonable thing for a
		// script to do, and answering it with status 1 ends one running
		// under `set -e`.
		return []string{"true"}
	}
	realLast, lastHist, search, base := fcPositions(all)

	// Operands are the numbers the listings show, which run ahead of list
	// positions once HISTSIZE has trimmed entries off the front (#277).
	// Anything past the second is ignored rather than refused, which is
	// bash's own reading — `fc -l one=two three=four 502` is a *prefix
	// search* for `one=two` that fails, not "too many arguments"
	// (history.tests, measured).
	first, last, swapped, kind := fcListRange(args, realLast, lastHist, search, base)
	if bad := fcReportNum(hc, kind); bad != nil {
		return bad
	}
	reverse = reverse || swapped
	for i := first; i <= last; i++ {
		j := i
		if reverse {
			j = last - (i - first)
		}
		writeFCLine(hc, j+base+1, all[j], numbered)
	}
	return []string{"true"}
}

// fcListRange resolves `fc -l`'s operands to a pair of 0-based positions
// and says whether the pair was written newest-first, which is a listing
// direction rather than a mistake to normalize away.
func fcListRange(args []string, realLast, lastHist int, search []string, base int) (first, last int, reverse bool, kind fcNum) {
	if len(args) == 0 {
		last = lastHist
		if first = last - fcDefaultList + 1; first < 0 {
			first = 0
		}
		return first, last, false, fcNumOK
	}
	if first, kind = fcHistNum(args[0], true, search, realLast, base, true, true); kind != fcNumOK {
		return 0, 0, false, kind
	}
	switch {
	case len(args) > 1:
		if last, kind = fcHistNum(args[1], true, search, realLast, base, true, false); kind != fcNumOK {
			return 0, 0, false, kind
		}
	case first == realLast:
		// `fc -l -0` names the fc line itself, and one operand naming it
		// lists that one line rather than running to the end of a list it
		// is already at the end of.
		last = realLast
	default:
		last = lastHist
	}
	// A negative position is clamped rather than refused, per POSIX: an
	// out-of-range specification is not an error for fc. The clamp has to
	// come before the indexing and after the swap below, which is the
	// order #277 got wrong — it clamped first and then carried an
	// unclamped operand into `last`, and the caller indexed past the end
	// and panicked, costing history.tests 368 of its 410 lines.
	if first < 0 {
		first = 0
	}
	if last < 0 {
		last = 0
	}
	if last < first {
		return last, first, true, fcNumOK
	}
	return first, last, false, fcNumOK
}

// fcPositions splits the history list the way fc reads it: realLast is
// the newest entry including the `fc` line itself, lastHist is the newest
// entry a specification can name, search is the slice a prefix search
// runs over, and base is the numbering offset trims have accumulated.
//
// bash's fc never names its own invocation — the entry is there, and
// last_hist steps over it — which is what makes a bare `fc -s` the
// previous command and `fc -l -0` the one shape that reaches past it.
func fcPositions(all []string) (realLast, lastHist int, search []string, base int) {
	realLast = len(all) - 1
	lastHist = realLast
	if historyAmbientLast() {
		lastHist--
	}
	if lastHist < 0 {
		lastHist = 0
	}
	return realLast, lastHist, all[:lastHist+1], historyBase()
}

// fcReportNum turns a specification that did not resolve into bash's
// diagnostic for it. nil means it did resolve.
func fcReportNum(hc interp.HandlerContext, kind fcNum) []string {
	switch kind {
	case fcNumInvalid:
		hc.Errf("fc: history specification out of range\n")
		return []string{"false"}
	case fcNumNotFound:
		hc.Errf("fc: no command found\n")
		return []string{"false"}
	}
	return nil
}

// fcExecute is `fc -s [pat=rep ...] [command]`, and `fc -e -` which is
// the same thing spelled the other way (#711).
//
// This is the form `alias r='fc -s'` exists for, and it is the half of
// the editing forms that does not need an editor. AGENTS.md recorded the
// whole family as deliberately absent because "re-executing a command
// belongs to the REPL's run loop rather than to a builtin" — which is
// still true, and is why nothing here runs anything. The builtin
// *resolves* the command and hands it back as an `eval`, which is the
// CallHandler rewrite `config` and `zi` already use: the interpreter runs
// it on its own path, so the status, the traps and the redirections are
// the ones a command run any other way would get, and `(exit 42); fc -s`
// answers 42 without the builtin knowing what 42 means.
//
// The editor form stays refused, and the reason is specific rather than
// general: bash echoes the edited file back as it reads it (readline's
// echo_input_at_read, which is `set -v`), and koi's `set -v` answers
// "cannot turn verbose on: not implemented" (#734) — so the output could
// not be matched even with the editor running.
func fcExecute(hc interp.HandlerContext, args []string) []string {
	// Leading `pat=rep` operands, in the order they were written; bash
	// applies each one to the whole command, globally.
	type substitution struct{ pat, rep string }
	var subs []substitution
	for len(args) > 0 {
		eq := strings.IndexByte(args[0], '=')
		if eq < 0 {
			break
		}
		subs = append(subs, substitution{args[0][:eq], args[0][eq+1:]})
		args = args[1:]
	}

	all := historyEntries()
	// bash's fc never re-runs its own line: the entry recording the `fc -s`
	// is left out of the search, which is what makes a bare `fc -s` the
	// previous command rather than an endless loop.
	realLast, _, entries, base := fcPositions(all)
	spec, haveSpec := "", false
	if len(args) > 0 {
		spec, haveSpec = args[0], true
	}
	idx, kind := fcHistNum(spec, haveSpec, entries, realLast, base, false, false)
	if kind != fcNumOK {
		// Every failure is the same message here, including a bad
		// specification: `fc -s -0` is an invalid position and still says
		// "no command found", because bash resolves it through fc_gethist,
		// which reports absence for anything it could not turn into an
		// index (measured — history5.sub tests exactly that line).
		hc.Errf("fc: no command found\n")
		return []string{"false"}
	}
	command := entries[idx]
	for _, s := range subs {
		if s.pat == "" {
			continue
		}
		command = strings.ReplaceAll(command, s.pat, s.rep)
	}
	// bash prints what it is about to run on **stderr** and with no
	// location prefix, which is a different channel from every diagnostic
	// beside it (measured with `fc -s a=x 2>/dev/null`).
	hc.RawErrf("%s\n", command)
	// And the command replaces the `fc -s` line in the history, so `r`
	// leaves the thing it re-ran rather than itself — through the same
	// HISTCONTROL gate an ordinary entry goes through, which is bash's
	// maybe_add_history.
	historyPopSelf()
	historyAppendFiltered(command, false, func(name string) string {
		return sessionVarOf(sessionRunner(), name)
	})
	// No `--` in front of it: koi's eval joins its operands into the
	// string it parses, so a `--` would be part of the command. bash
	// hands the string to its parser the same way.
	return []string{"eval", command}
}

// fcCheckEditSpec reads the editor form's range operands only to find out
// whether they are readable at all. bash resolves them before it opens a
// temp file, so a bad one is `history specification out of range` rather
// than anything about editors — which is what `fc -0` is.
func fcCheckEditSpec(hc interp.HandlerContext, args []string) ([]string, bool) {
	all := historyEntries()
	if len(all) == 0 {
		return nil, false
	}
	realLast, _, entries, base := fcPositions(all)
	for i, arg := range args {
		if i > 1 {
			break
		}
		_, kind := fcHistNum(arg, true, entries, realLast, base, false, i == 0)
		if bad := fcReportNum(hc, kind); bad != nil {
			return bad, true
		}
	}
	return nil, false
}

// fcNum says how a specification resolved: to a position, to something
// that is not one, or to a prefix nothing in the list starts with. bash
// keeps these apart as three sentinel indices and its callers read them
// differently — the execute form calls all three "no command found"
// while the listing form has a message per kind.
type fcNum int

const (
	fcNumOK fcNum = iota
	fcNumInvalid
	fcNumNotFound
)

// fcHistNum is bash's fc_gethnum: a specification resolved against the
// list to a 0-based position. entries excludes the fc line itself;
// realLast is the last position *including* it, which only `-0` while
// listing ever names.
//
// first marks the first of a pair, which changes only what an
// out-of-range absolute number answers — bash returns the oldest entry
// for the start of a range and the newest for its end, so a range wider
// than the list is the whole list rather than an error.
func fcHistNum(spec string, have bool, entries []string, realLast, base int, listing, first bool) (int, fcNum) {
	last := len(entries) - 1
	if last < 0 {
		return 0, fcNumNotFound
	}
	// No specification is the most recent command.
	if !have {
		return last, fcNumOK
	}
	s, sign := spec, 1
	if strings.HasPrefix(s, "-") {
		sign, s = -1, s[1:]
	}
	if s != "" && s[0] >= '0' && s[0] <= '9' {
		n := fcAtoiPrefix(s) * sign
		switch {
		case n < 0:
			// Negative is an offset from the current position, clamped
			// rather than refused: `fc -s -- -42` on a three-entry list is
			// the oldest entry, not an error.
			if n += last + 1; n < 0 {
				n = 0
			}
			return n, fcNumOK
		case n == 0:
			// `0` is the most recent command and `-0` is not the same
			// thing: while listing it names the fc line itself, and while
			// editing it is not a position at all.
			if sign == -1 {
				if listing {
					return realLast, fcNumOK
				}
				return 0, fcNumInvalid
			}
			return last, fcNumOK
		default:
			// An absolute number, in the coordinates the listing prints.
			// koi's base counts trims where bash's history_base counts
			// entries, so the two differ by one.
			if n -= base + 1; n < 0 || n >= last {
				if first {
					return 0, fcNumOK
				}
				return last, fcNumOK
			}
			return n, fcNumOK
		}
	}
	// Anything else is the most recent command starting with that text.
	for j := last; j >= 0; j-- {
		if strings.HasPrefix(entries[j], spec) {
			return j, fcNumOK
		}
	}
	return 0, fcNumNotFound
}

// fcAtoiPrefix is C's atoi: read the leading digits and stop, which is
// what makes `fc -e - 48` a position rather than a parse failure.
func fcAtoiPrefix(s string) int {
	n := 0
	for i := 0; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		n = n*10 + int(s[i]-'0')
		if n > 1<<40 {
			n = 1 << 40
		}
	}
	return n
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
