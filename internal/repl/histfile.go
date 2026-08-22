package repl

// $HISTFILE for the script paths (#432).
//
// `history -a` and `-n` used to refuse: both mean "the lines new since
// last time", which needs a read position over a per-process list, and
// koi's history was a store shared live across sessions (#40) where
// "new since" has no single answer. The ambient-recording work (#277)
// gave a *script* session a list of its own with bash's append, trim and
// numbering semantics, so the objection no longer holds there — and the
// interactive posture is unchanged, which is what histAmbientSession
// gates.
//
// # The one counter
//
// Everything here is bash's single history_lines_in_file, measured
// against bash 5.3 rather than read out of its source:
//
//   - the preload at `set -o history` sets it to the lines it read,
//   - `-a` adds the lines it wrote,
//   - `-n` reads the file from it and sets it to the file's length,
//   - `-r` sets it to the length of whatever file it read.
//
// That is why `history -a; history -n` reads nothing: the append moved
// the same counter the read consults. Two shapes look symmetric and are
// not, so both are pinned by tests: `-r` marks everything before its
// read as already-appended (a later `-a` writes the read lines out
// again), while `-n` leaves the append position alone (a later `-a`
// writes the pre-read entries *and* the read lines).
//
// # HISTFILESIZE
//
// Assigning HISTFILESIZE truncates $HISTFILE on the spot — a write to a
// file the script never asked to write, which is why it was left out
// until it could be measured (#491). Two rules, both measured against
// bash 5.3 rather than reasoned from the manual:
//
//   - Assigning HISTFILESIZE truncates the file to that many lines
//     immediately, whether or not history is on. Zero empties it.
//   - Enabling history truncates to HISTFILESIZE, which defaults to
//     HISTSIZE when unset. That is why `HISTFILE=f; HISTSIZE=3; set -o
//     history` truncates and `set -o history; HISTSIZE=3` does not:
//     assigning HISTSIZE is not itself an action.

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// historyEnableCallHandler preloads $HISTFILE when `set -o history`
// turns recording on.
//
// The moment matters and is measured: bash reads the file as the option
// flips, so `HISTFILE=hf; set -o history` loads hf while `set -o
// history; HISTFILE=hf` loads nothing. Doing it here rather than lazily
// at the first recorded line is what preserves that — by the time a line
// is recorded, HISTFILE may already name a different file.
//
// The call is passed through untouched: the interpreter still owns the
// option itself.
func historyEnableCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if len(args) == 3 && args[0] == "set" && args[1] == "-o" && args[2] == "history" {
			// Truncation comes first: bash trims the file as the option
			// flips, so what the preload reads is the trimmed file.
			historyTruncateFile("")
			historyPreload(interp.HandlerCtx(ctx))
		}
		return next(ctx, args)
	}
}

// historyTruncateFile trims $HISTFILE to its last n lines, where n is
// the given value, or HISTFILESIZE, or HISTSIZE (#491).
//
// It is called for an assignment to HISTFILESIZE, where the value is
// the one just assigned, and when history is enabled, where it is not.
func historyTruncateFile(assigned string) {
	runner := sessionRunner()
	path := sessionVarOf(runner, "HISTFILE")
	if path == "" {
		return
	}
	raw := assigned
	if raw == "" {
		raw = sessionVarOf(runner, "HISTFILESIZE")
	}
	if raw == "" {
		raw = sessionVarOf(runner, "HISTSIZE")
	}
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit < 0 {
		// A value that is not a number is not a limit; bash leaves the
		// file alone rather than emptying it.
		return
	}
	// Against the *shell's* directory, not the Go process's: they differ
	// after a `cd`, and a relative HISTFILE truncated against the wrong
	// one would shorten whatever file happened to share the name there.
	// bash resolves it at the moment it writes (measured: `HISTFILE=./h;
	// set -o history; cd sub` writes sub/h).
	path = historyFilePathIn(runnerDir(runner), path)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if len(lines) <= limit {
		return
	}
	out := ""
	if limit > 0 {
		out = strings.Join(lines[len(lines)-limit:], "\n") + "\n"
	}
	// Written in place rather than through a temporary: the file's
	// identity is what $HISTFILE names, and a rename would break a
	// shell that has it open.
	_ = os.WriteFile(path, []byte(out), 0o600)
}

// historyFileSizeHook is what the interpreter calls when HISTFILESIZE
// is assigned.
func historyFileSizeHook(_ string, value string) {
	historyTruncateFile(value)
}

// historyPreload loads $HISTFILE into the session list, once per shell.
func historyPreload(hc interp.HandlerContext) {
	histMu.Lock()
	if histPreloaded || !histAmbientSession {
		histMu.Unlock()
		return
	}
	histPreloaded = true
	histMu.Unlock()

	runner := sessionRunner()
	path := sessionVarOf(runner, "HISTFILE")
	if path == "" {
		return
	}
	lines, err := historyFileLines(hc, path)
	if err != nil {
		// bash says nothing when the file is not there — a shell that
		// complained would break every rc that enables history before
		// the file exists.
		return
	}

	histMu.Lock()
	defer histMu.Unlock()
	histFileLines = len(lines)
	histList = lines
	histMutated = true
	// Every line gets a number before HISTSIZE drops any: a five-line
	// file under HISTSIZE=2 lists its last two as 5 and 6, not as 1 and
	// 2. The trim here advances the base by the whole drop, unlike the
	// assignment-shrink rule histTrimLocked carries (which renumbers one
	// lower) — both measured, and the difference is why this does not
	// just call that.
	if n, ok := historySizeLimit(runner); ok {
		if drop := len(histList) - n; drop > 0 {
			histList = histList[drop:]
			histBase += drop
		}
	}
	histAppendPos = histBase + len(histList)
}

// historySizeLimit reads HISTSIZE as the list's cap. Unset means
// unlimited in a non-interactive shell — bash's 500 is an interactive
// default — and a negative or unparsable value means no cap either.
func historySizeLimit(runner *interp.Runner) (int, bool) {
	raw := sessionVarOf(runner, "HISTSIZE")
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// historyFileLines reads a history file as one command per line,
// resolving a relative path against the *shell's* directory.
func historyFileLines(hc interp.HandlerContext, path string) ([]string, error) {
	f, err := os.Open(historyFilePath(hc, path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// historyFilePath resolves against the shell's directory, not the Go
// process's. They differ after a `cd`, which is most scripts.
func historyFilePath(hc interp.HandlerContext, path string) string {
	return historyFilePathIn(hc.Dir, path)
}

// historyFilePathIn is historyFilePath for a caller that has the
// runner rather than a handler context — the exit write (#737) happens
// after the last statement, where there is no statement to have one.
func historyFilePathIn(dir, path string) string {
	if filepath.IsAbs(path) || dir == "" {
		return path
	}
	return filepath.Join(dir, path)
}

// runnerDir is the shell's current directory. It is a live field: `cd`
// assigns it as it runs, so it is current at exit as well as mid-script.
func runnerDir(runner *interp.Runner) string {
	if runner == nil {
		return ""
	}
	return runner.Dir
}

// historyAppendNew is `history -a`: write the entries recorded since the
// last append, then mark them written.
//
// A position *behind* the list start means entries it named are gone —
// `history -c` after a preload is the way there — and bash writes the
// whole list rather than nothing (measured), which is what clamping to
// zero produces.
func historyAppendNew(hc interp.HandlerContext, path string) error {
	histMu.Lock()
	defer histMu.Unlock()
	if !histMutated {
		histList = historyEntriesLocked()
		histMutated = true
	}
	start := max(histAppendPos-histBase, 0)
	start = min(start, len(histList))
	pending := histList[start:]

	f, err := os.OpenFile(historyFilePath(hc, path), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var b strings.Builder
	for _, cmd := range pending {
		b.WriteString(cmd)
		b.WriteByte('\n')
	}
	if _, err := f.WriteString(b.String()); err != nil {
		return err
	}
	histAppendPos = histBase + len(histList)
	histFileLines += len(pending)
	return nil
}

// historyReadNew is `history -n`: read the lines this session has not
// read yet and append them to the list.
//
// It leaves the append position alone, which is not symmetric with `-r`
// and is what bash does: a `-n` followed by a `-a` writes the read lines
// back out to the same file, duplicating them. Measured rather than
// reasoned, and pinned by a test so the asymmetry cannot be tidied away.
func historyReadNew(hc interp.HandlerContext, path string, runner *interp.Runner) error {
	lines, err := historyFileLines(hc, path)
	if err != nil {
		return err
	}
	histMu.Lock()
	defer histMu.Unlock()
	if !histMutated {
		histList = historyEntriesLocked()
		histMutated = true
	}
	if histFileLines < len(lines) {
		histList = append(histList, lines[histFileLines:]...)
		histTrimLocked(sessionVarOf(runner, "HISTSIZE"))
	}
	histFileLines = len(lines)
	histAmbientLast = false
	return nil
}

// historyReadAll is the bookkeeping half of `history -r`: the read
// marks everything already in the list as written, so a later `-a`
// writes the lines just read (and not what preceded them) — measured.
func historyReadAll(lines []string, runner *interp.Runner) {
	histMu.Lock()
	defer histMu.Unlock()
	if !histMutated {
		histList = historyEntriesLocked()
		histMutated = true
	}
	histAppendPos = histBase + len(histList)
	histFileLines = len(lines)
	histList = append(histList, lines...)
	histTrimLocked(sessionVarOf(runner, "HISTSIZE"))
	histAmbientLast = false
}

// historySaveAtExit writes $HISTFILE when the session ends (#737).
//
// It is bash's maybe_save_shell_history, and bash does it for a
// **non-interactive** shell too — its own history5.sub ends `unset
// HISTFILE  # suppress writing history file`, which is only meaningful
// if the write happens. koi never wrote the file on any path, so the
// round trip #432 opened was one-way.
//
// The gates, all measured against bash 5.3.15 rather than assumed:
//
//   - `set -o history` must be on **at exit** — `set +o history` before
//     the end suppresses the write entirely, and a shell that never
//     turned it on writes nothing;
//   - $HISTFILE must be set at exit, read then rather than earlier, so
//     `unset HISTFILE` suppresses it and reassigning it writes to the
//     new file. koi has no default here: bash's ~/.bash_history fallback
//     is installed for *interactive* shells only, so a non-interactive
//     bash with HISTFILE unset writes nothing either (measured with a
//     seeded ~/.bash_history, which came back untouched);
//   - it happens **after** the EXIT trap, so `trap 'rm -f $HISTFILE' 0`
//     does not stop it — the file is recreated;
//   - and it is then truncated to $HISTFILESIZE (or HISTSIZE), which is
//     what historyTruncateFile already does for #491.
//
// # Why this cannot overwrite someone's history file
//
// bash writes the whole list only when the entries recorded this session
// outnumber the entries still in it — which needs a HISTSIZE trim to
// have dropped entries that were never written — and `shopt -s
// histappend` turns even that into an append. Everywhere else it
// *appends* the entries recorded since the last write, which is the same
// accounting `history -a` already has here.
//
// So the branch is pending > len(list), and it is exactly the case
// historyAppendNew already clamps to zero: the append position is behind
// the list start. `histappend` is `optStateOnly` in the interpreter's
// table (#575), which means "the bit is tracked and answered; the
// behavior it names belongs to the shell around the interpreter" — and
// this is that shell, so the bit is read here through
// [interp.Runner.OptionSet] and the table is left alone.
//
// The list is never seeded from the store on this path: an ambient
// session has no store (historyStore is opened by the interactive loop
// alone), and if that ever changed, writing the cross-session store
// (#40) into someone's $HISTFILE would be the worst available answer. An
// unmutated list therefore means nothing was recorded, which is also
// when bash writes nothing.
func historySaveAtExit(runner *interp.Runner) {
	if runner == nil || !runner.OptionSet("history") {
		return
	}
	histMu.Lock()
	// Only a session with a per-process list (#277) has anything to
	// write; an interactive session's history is the shared store, and
	// its own `set -o history` posture is untouched by this.
	if !histAmbientSession || !histMutated {
		histMu.Unlock()
		return
	}
	pending := histBase + len(histList) - histAppendPos
	list := append([]string(nil), histList...)
	histMu.Unlock()

	if pending <= 0 {
		// Nothing recorded since the last `history -a` or `-c`, which is
		// bash's `history_lines_this_session > 0` gate.
		return
	}
	path := sessionVarOf(runner, "HISTFILE")
	if path == "" {
		return
	}
	path = historyFilePathIn(runnerDir(runner), path)

	if pending > len(list) && !runner.OptionSet("histappend") {
		historyWriteWhole(path, list)
	} else {
		historyAppendTail(path, list, min(pending, len(list)))
	}
	// bash truncates after the write, to HISTFILESIZE or, unset, to
	// HISTSIZE — which is why `HISTSIZE=1` leaves a one-line file even
	// under histappend.
	historyTruncateFile("")
}

// historyWriteWhole replaces the file with the list, one command per
// line. Failures are silent, as bash's are: an unwritable $HISTFILE at
// exit prints nothing and does not change the status (measured).
func historyWriteWhole(path string, list []string) {
	var b strings.Builder
	for _, cmd := range list {
		b.WriteString(cmd)
		b.WriteByte('\n')
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o600)
}

// historyAppendTail appends the newest n entries to the file, creating
// it when it is not there — which is what makes `trap 'rm -f $HISTFILE'
// 0` recreate the file with only this session's lines in it.
func historyAppendTail(path string, list []string, n int) {
	if n <= 0 {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // exit path; a failed write is silent in bash too
	var b strings.Builder
	for _, cmd := range list[len(list)-n:] {
		b.WriteString(cmd)
		b.WriteByte('\n')
	}
	_, _ = f.WriteString(b.String())
}

// historyNoFile reports the missing-HISTFILE case the way bash does,
// naming the variable rather than the flag.
func historyNoFile(hc interp.HandlerContext, flag string) []string {
	hc.Errf("history: %s: no file given and HISTFILE is unset\n", flag)
	return historyStatus(1)
}
