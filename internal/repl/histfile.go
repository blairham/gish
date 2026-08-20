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
// # Not implemented, deliberately
//
// HISTFILESIZE truncation. Assigning HISTSIZE with HISTFILESIZE unset
// makes bash truncate $HISTFILE on the spot — a write to a file the
// script never asked to write — and that is its own feature with its own
// measurements rather than a detail of these three.

import (
	"bufio"
	"context"
	"fmt"
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
			historyPreload(interp.HandlerCtx(ctx))
		}
		return next(ctx, args)
	}
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
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(hc.Dir, path)
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

// historyNoFile reports the missing-HISTFILE case the way bash does,
// naming the variable rather than the flag.
func historyNoFile(hc interp.HandlerContext, flag string) []string {
	fmt.Fprintf(hc.Stderr, "history: %s: no file given and HISTFILE is unset\n", flag)
	return historyStatus(1)
}
