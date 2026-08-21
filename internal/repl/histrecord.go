package repl

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/pattern"
	"github.com/blairham/koi-shell/internal/shell/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// Ambient history recording (#277): under `set -o history` bash records
// each command as it executes, and the entry text is the *raw source*, not
// a pretty-printing — `for x in one two three\ndo\n\t:\ndone` is recorded
// as `for x in one two three; do 	:; done`, tab intact. That rules out
// rendering the syntax tree: the recorder keeps the source bytes and
// slices them by statement position, joining lines with bash's measured
// rules. The interpreter's HistoryHook fires per top-level statement,
// before it runs; everything else — grouping, joining, HISTCONTROL,
// HISTIGNORE, HISTSIZE — lives here, where the source and the `history`
// builtin's session list both are.
//
// The unit is the *line*, not the statement: `echo a; echo b` is one bash
// entry, and `done; echo tail` folds the trailing command into the loop's
// entry. Statements are therefore grouped up front — consecutive
// statements whose start line falls inside the group's span share one
// entry — and the hook's job is just to say "this group is now running".
//
// The map is keyed by statement pointer so a statement from some *other*
// parse — the rc file `-i` sources, a profile — is silently ignored
// rather than sliced against the wrong source, which also matches bash:
// rc commands are not recorded.

// histGroup is one history entry to be: a span of source lines and
// whether the entry ends inside a heredoc (which bash records with a
// trailing newline after the delimiter — measured via `history -w`).
type histGroup struct {
	start, end uint
	hdocTail   bool
}

type historyRecorder struct {
	// src reads the script source as it has been read so far. The shell
	// reads and runs a line at a time, so the source grows under the
	// recorder; splitting it into lines is deferred to record time,
	// where it is only paid when `set -o history` is actually on.
	src    func() string
	srcLen int
	lines  []string // source split on \n; line n is lines[n-1]
	groups []histGroup
	byStmt map[*syntax.Stmt]int
	done   []bool
	// sep[l] is the separator to write between line l and line l+1 when
	// they are joined into one entry, for the boundaries that are inside
	// a multi-line construct. Everything not listed here uses the
	// line-ending rules in joinLine.
	sep map[uint]string
	// env reads a session variable as the script sees it right now
	// (interp.LookupVar), because HISTCONTROL, HISTIGNORE and HISTSIZE
	// are read at record time and a script sets them as it goes.
	env func(string) string
}

// sessionVarOf reads name from the runner the way the running script
// sees it. Not runner.Vars: that map is only synced when Run returns,
// and the recorder reads HISTCONTROL mid-run, right after the script
// assigned it.
func sessionVarOf(runner *interp.Runner, name string) string {
	if runner == nil {
		return ""
	}
	if v := runner.LookupVar(name); v.IsSet() {
		return v.String()
	}
	return ""
}

func newHistoryRecorder(src func() string, env func(string) string) *historyRecorder {
	return &historyRecorder{
		src:    src,
		byStmt: make(map[*syntax.Stmt]int),
		sep:    make(map[uint]string),
		env:    env,
	}
}

// restart drops what the recorder knows and reads from src instead,
// which is what a switch of the command stream calls for (#516): the new
// stream has its own line numbering, and everything from the old one has
// already run.
func (rec *historyRecorder) restart(src func() string) {
	rec.src, rec.srcLen, rec.lines = src, 0, nil
	rec.groups, rec.done = nil, nil
	clear(rec.byStmt)
	clear(rec.sep)
}

// sourceLines is the source split into lines, resplit only when the
// source has grown since the last look.
func (rec *historyRecorder) sourceLines() []string {
	if s := rec.src(); len(s) != rec.srcLen {
		rec.srcLen = len(s)
		rec.lines = strings.Split(s, "\n")
	}
	return rec.lines
}

// addLine records the statements of one input line as one history entry
// to be. The shell hands them over before running them, which is also
// the order the hook fires in.
func (rec *historyRecorder) addLine(stmts []*syntax.Stmt) {
	if len(stmts) == 0 {
		return
	}
	g := histGroup{start: stmts[0].Pos().Line()}
	for _, st := range stmts {
		if end := stmtEndLine(st); end > g.end {
			g.end = end
			g.hdocTail = stmtEndsInHdoc(st, end)
		}
		rec.markBoundaries(st)
	}
	rec.groups = append(rec.groups, g)
	rec.done = append(rec.done, false)
	for _, st := range stmts {
		rec.byStmt[st] = len(rec.groups) - 1
	}
}

// markBoundaries records how the lines a statement spans are joined.
//
// Line boundaries inside a multi-line construct don't take the
// line-ending separators: bash joins them by parser state, and the three
// states were measured — an open quote or command substitution keeps its
// newline, a compound assignment collapses to a space, and a heredoc
// keeps its newlines plus one after the closing delimiter. Walk visits
// parents before children, so an inner construct's boundaries overwrite
// the outer one's, which is also bash's answer (the innermost state is
// the parser's state).
func (rec *historyRecorder) markBoundaries(node syntax.Node) {
	syntax.Walk(node, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.SglQuoted, *syntax.DblQuoted, *syntax.CmdSubst:
			markRegion(rec.sep, n.Pos().Line(), x.End().Line(), "\n")
		case *syntax.ArrayExpr:
			markRegion(rec.sep, x.Pos().Line(), x.End().Line(), " ")
		case *syntax.Redirect:
			if x.Hdoc != nil {
				// The body starts on the line after the operator and
				// Hdoc.End() lands on the closing delimiter's line, so
				// every boundary from the redirect's line down to the
				// delimiter joins with a newline.
				markRegion(rec.sep, x.OpPos.Line(), x.Hdoc.End().Line(), "\n")
			}
		}
		return true
	})
}

func markRegion(sep map[uint]string, start, end uint, s string) {
	for l := start; l < end; l++ {
		sep[l] = s
	}
}

// stmtEndLine is the last source line a statement occupies. Stmt.End()
// does not include heredoc bodies — they hang off redirects with their
// own positions — so those are folded in by hand.
func stmtEndLine(st *syntax.Stmt) uint {
	end := st.End().Line()
	syntax.Walk(st, func(n syntax.Node) bool {
		if rd, ok := n.(*syntax.Redirect); ok && rd.Hdoc != nil {
			if l := rd.Hdoc.End().Line(); l > end {
				end = l
			}
		}
		return true
	})
	return end
}

// stmtEndsInHdoc reports whether line end is a heredoc's closing
// delimiter, which is the one case where the recorded entry keeps a
// trailing newline (measured byte-for-byte via `history -w`).
func stmtEndsInHdoc(st *syntax.Stmt, end uint) bool {
	tail := false
	syntax.Walk(st, func(n syntax.Node) bool {
		if rd, ok := n.(*syntax.Redirect); ok && rd.Hdoc != nil && rd.Hdoc.End().Line() == end {
			tail = true
		}
		return true
	})
	return tail
}

// record is the HistoryHook target: called with each top-level statement
// about to run while `set -o history` is on.
func (rec *historyRecorder) record(st *syntax.Stmt) {
	gi, ok := rec.byStmt[st]
	if !ok || rec.done[gi] {
		return
	}
	rec.done[gi] = true
	historyAppendFiltered(rec.render(rec.groups[gi]), true, rec.env)
}

// render joins the group's source lines into the entry text bash would
// record.
func (rec *historyRecorder) render(g histGroup) string {
	var b strings.Builder
	lines := rec.sourceLines()
	for l := g.start; l <= g.end; l++ {
		if int(l) > len(lines) {
			break // a truncated read; record what there is
		}
		line := lines[l-1]
		if l == g.end {
			b.WriteString(line)
			break
		}
		if sep, ok := rec.sep[l]; ok {
			b.WriteString(line)
			b.WriteString(sep)
			continue
		}
		b.WriteString(joinLine(line))
	}
	if g.hdocTail {
		// bash keeps the newline after the closing delimiter.
		b.WriteString("\n")
	}
	return b.String()
}

// joinLine returns line plus the separator bash puts between it and the
// next line of the same entry, all measured against bash 5.3:
//
//   - a trailing backslash is a continuation: the backslash is dropped
//     and nothing is inserted (`echo one \` + `two` → `echo one two`);
//   - a line ending in an operator or a keyword that cannot take a `;`
//     joins with a space (`if true; then` + `echo hi` → `then echo hi`,
//     and likewise after do, else, `&&`, `|`, `{`, `;;`, `in`, …);
//   - everything else joins with "; " (`for x in one two three` + `do`
//     → `three; do`).
func joinLine(line string) string {
	if strings.HasSuffix(line, "\\") {
		// Only a backslash immediately before the newline continues the
		// line; the parser guaranteed that by putting both lines in one
		// statement with no construct spanning the boundary.
		return line[:len(line)-1]
	}
	t := strings.TrimRight(line, " \t")
	for _, s := range []string{"&&", "||", "|", "&", ";", "{", "("} {
		if strings.HasSuffix(t, s) {
			return line + " "
		}
	}
	switch lastWord(t) {
	case "then", "do", "else", "elif", "in", "time", "!":
		return line + " "
	}
	return line + "; "
}

func lastWord(s string) string {
	if i := strings.LastIndexAny(s, " \t"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// historyAppendFiltered runs one would-be entry through bash's filters
// and, if it survives, appends it to the session list and applies
// HISTSIZE. `history -s` goes through the same gate: measured, bash
// filters those too — `history -s ' spaced'` under ignoreboth records
// nothing. ambient marks the entry as the record of the line currently
// executing, which is what `history -s`/-p replace.
func historyAppendFiltered(entry string, ambient bool, env func(string) string) {
	ctl := env("HISTCONTROL")
	ignoreSpace, ignoreDups, eraseDups := false, false, false
	for f := range strings.SplitSeq(ctl, ":") {
		switch f {
		case "ignorespace":
			ignoreSpace = true
		case "ignoredups":
			ignoreDups = true
		case "ignoreboth":
			ignoreSpace, ignoreDups = true, true
		case "erasedups":
			eraseDups = true
		}
	}
	// A space, not any whitespace: a tab-led line is recorded (measured).
	if ignoreSpace && strings.HasPrefix(entry, " ") {
		return
	}

	histMu.Lock()
	defer histMu.Unlock()
	// A filtered entry is not the record of the running line, so nothing
	// for `history -s` to replace — bash has the same answer when
	// HISTIGNORE ate the -s line itself.
	histAmbientLast = false
	list := historyEntriesLocked()
	last := ""
	if len(list) > 0 {
		last = list[len(list)-1]
	}
	if ignoreDups && entry == last {
		return
	}
	if histIgnored(entry, last, env("HISTIGNORE")) {
		return
	}

	if !histMutated {
		histList = list
		histMutated = true
	}
	if eraseDups {
		kept := histList[:0]
		for _, e := range histList {
			if e != entry {
				kept = append(kept, e)
			}
		}
		histList = kept
	}
	histList = append(histList, entry)
	histAmbientLast = ambient
	histTrimLocked(env("HISTSIZE"))
}

// histIgnored answers HISTIGNORE: colon-separated shell patterns matched
// against the whole entry, plus `&` for "same as the previous entry".
func histIgnored(entry, last, spec string) bool {
	if spec == "" {
		return false
	}
	for pat := range strings.SplitSeq(spec, ":") {
		if pat == "" {
			continue
		}
		if pat == "&" {
			if entry == last {
				return true
			}
			continue
		}
		expr, err := pattern.Regexp(pat, pattern.EntireString)
		if err != nil {
			continue // an unparsable pattern matches nothing
		}
		if regexp.MustCompile(expr).MatchString(entry) {
			return true
		}
	}
	return false
}

// histTrimLocked applies HISTSIZE to the session list. Requires histMu.
//
// Unset means unlimited — bash sets its 500 default only for interactive
// shells, and `env -i bash script.sh` reports HISTSIZE as unset while
// recording without a cap (measured). The dropped count feeds histBase so
// listing numbers keep advancing past a trim, which is also measured:
// after HISTSIZE=2 trims two entries the next listing starts at 3.
//
// The drop>1 branch is bash's assignment-time truncation quirk. bash
// truncates the moment HISTSIZE is assigned, and that truncation
// renumbers the survivors one *lower* than they were, while the ordinary
// one-at-a-time stifle preserves numbers. koi trims lazily on the next
// append, so a multi-entry drop here is exactly the "an assignment
// shrank the cap since last append" case, and the -1 reproduces bash's
// numbering — measured three ways: shrink after -c, shrink mid-list,
// and shrink-then-grow all agree.
func histTrimLocked(size string) {
	if size == "" {
		return
	}
	n, err := strconv.Atoi(size)
	if err != nil || n < 0 {
		return // bash treats a negative size as unlimited
	}
	drop := len(histList) - n
	if drop <= 0 {
		return
	}
	histList = append([]string(nil), histList[drop:]...)
	if drop > 1 {
		histBase += drop - 1
	} else {
		histBase += drop
	}
}
