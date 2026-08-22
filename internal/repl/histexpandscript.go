package repl

import (
	"fmt"
	"strings"

	"github.com/blairham/koi-shell/internal/history"
	"github.com/blairham/koi-shell/internal/shell/interp"
)

// History expansion in a script (#559).
//
// `set -H` was refused, so `!!` in a script was an ordinary command name
// and everything after the line that asked for it diverged from bash.
// The expander itself has been complete since #96 — designators,
// modifiers, chaining, the lexical outs — and ran only on the raw
// interactive line; what was missing was the line boundary in a script
// to hang it on, which #450 built and #619 has since used twice.
//
// So this is wiring, and the wiring is four measured rules.
//
// **The gate is two options, not one.** bash needs `set -H` *and*
// `set -o history`, which is not documented anywhere and is measurable in
// both directions: `set -H` alone leaves `!!` a command name, and
// `set +o history` with both on stops the expansion rather than merely
// stopping the recording. That is because the expansion reads the history
// list, and the option that fills it is what says the list exists.
//
// **The unit is the physical line**, which is #450's rule and had to be
// measured again here rather than inferred: `set -H; echo !!` on one line
// prints `!!`, because the option is read before the line runs, while a
// continuation line of a compound command *is* expanded and echoed on its
// own. A here-document body is the one input the shell hands over
// untouched.
//
// **An expansion is echoed to standard error**, not to standard output —
// the opposite of koi's interactive echo, which prints to stdout so it
// lands in the terminal's scrollback beside the command. bash's script
// echo is a diagnostic and goes where diagnostics go, which is also what
// keeps `x=$(cmd)` from capturing it.
//
// **A refused expansion costs the line, and the line's number with it.**
// `!nosuch` is `event not found`, and then the whole line is dropped —
// `echo mid; !nosuch; echo tail` runs neither of its echoes — while `$?`
// is left exactly as the previous command set it, so the filter answers
// with nothing rather than with an error: nothing is parsed, nothing
// runs, and the status nobody set stays unset. bash also steps its line
// counter *back* over such a line, which is measured and deliberate on
// its side (one explicit decrement when an expansion comes out empty),
// so everything after one is numbered one lower — `$LINENO` included.

// historyExpandFilter is the [interp.LineFilter] that expands a script's
// lines. Everything it needs comes from the runner: the two options, the
// stderr in force, and whether there is a file to name in a diagnostic at
// all — which is the shell's own rule (#571) rather than a second one.
//
// It is also where the recorder learns that a line was *read*, which is
// the only place that can be known (#693): bash adds a line to history in
// its reader, so a line that yields no statement — a comment — is an
// entry there, and whether it is one depends on `set -o history` at the
// moment it was read rather than at the moment something ran.
func historyExpandFilter(runner *interp.Runner, rec *historyRecorder) interp.LineFilter {
	// dropped counts the lines expansion has taken out of the input, so
	// the diagnostic can name the line bash names. bash's line counter
	// steps *back* over a line whose expansion came out empty — it is one
	// explicit decrement in its own reader, so it is a rule rather than
	// an accident, and it means everything after a refused expansion is
	// numbered one lower: `$LINENO`, the next command-not-found, all of
	// it. Handing the parser nothing at all is what reproduces that here
	// rather than reproducing it twice in two counters.
	dropped := 0
	return func(line string, num int) string {
		// The line's number in the parser's own coordinates, which is
		// what the recorder and the diagnostic both name — a line
		// expansion dropped never reaches the parser, so it costs one.
		history := runner.OptionSet("history")
		if rec != nil {
			rec.readLine(uint(num-dropped), history)
		}
		if !runner.OptionSet("histexpand") || !history {
			return line
		}
		body, nl := splitLineEnd(line)
		if body == "" {
			return line
		}
		expanded, changed, printOnly, err := expandHistoryLine(body, sessionHistorySource(), sessionHistChars(runner))
		if err != nil {
			fmt.Fprintf(runner.Stderr(), "%s%v\n", runner.InputLocation(num-dropped), err)
			dropped++
			return ""
		}
		if !changed {
			return line
		}
		fmt.Fprint(runner.Stderr(), expanded+"\n")
		if printOnly {
			// `:p` asks to be shown rather than run, which is the whole
			// reason to write it. The showing is the echo above — bash
			// prints it once, not twice — and the line then leaves the
			// input the way a refused one does, which is why it costs the
			// counter a line too (measured).
			dropped++
			return ""
		}
		return expanded + nl
	}
}

// splitLineEnd separates a line from the newline that ended it, which may
// be absent on the last line of a file.
func splitLineEnd(line string) (body, end string) {
	if rest, ok := strings.CutSuffix(line, "\n"); ok {
		return rest, "\n"
	}
	return line, ""
}

// sessionHistorySource is the list the expander looks in.
//
// The session list is the one the `history` builtin reports, so what a
// script recalls with `!!` and what `history` prints cannot disagree —
// and a script's entries get there through the same ambient recording
// `set -o history` turns on (#277), which fires before a statement runs
// and so has already recorded the line above the one being expanded.
//
// The numbering is the listing's own, `historyBase` plus the index, which
// is what makes `!2` and the `2` in `history`'s output the same entry
// even after HISTSIZE has trimmed the front off the list (#692).
func sessionHistorySource() histSource {
	entries := historyEntries()
	base := historyBase()
	return histSource{
		prefix: func(prefix string, n int) (string, bool) {
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
		},
		search: func(s string) (string, bool) {
			for i := len(entries) - 1; i >= 0; i-- {
				if strings.Contains(entries[i], s) {
					return entries[i], true
				}
			}
			return "", false
		},
		numbered: func(n int) (string, bool) {
			i := n - base - 1
			if i < 0 || i >= len(entries) {
				return "", false
			}
			return entries[i], true
		},
	}
}

// storeHistorySource is the interactive session's list: the editor's
// shared store (#40) rather than the interpreter's ambient one, which is
// why `set -o history` is not a gate on the interactive path. The
// numbering comes from what the `history` builtin prints, so `!2` and the
// `2` in its listing name the same entry there too.
func storeHistorySource(store *history.Store) histSource {
	entries := historyEntries()
	base := historyBase()
	return histSource{
		prefix: store.Match,
		search: func(s string) (string, bool) { return store.Search(s, 0) },
		numbered: func(n int) (string, bool) {
			i := n - base - 1
			if i < 0 || i >= len(entries) {
				return "", false
			}
			return entries[i], true
		},
	}
}

// sessionHistChars reads $histchars as the running script sees it, which
// is what makes it a live value rather than a startup one (#695).
func sessionHistChars(runner *interp.Runner) histChars {
	if runner == nil {
		return defaultHistChars
	}
	v := runner.LookupVar("histchars")
	chars := histCharsOf(v.String(), v.IsSet())
	// readline's history_quotes_inhibit_expansion, which bash turns on
	// with `set -o posix` — so under posix a `!` inside double quotes is
	// ordinary text while one outside them still expands (measured).
	chars.quotesInhibit = runner.OptionSet("posix")
	return chars
}
