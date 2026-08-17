package repl

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/blocks"
	"github.com/blairham/gish/internal/history"
	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/internal/ui"
)

// The ctrl-r picker and the `pick` builtin (#100): one selection
// primitive, two surfaces. fzf's install count says shells are missing
// this; gish ships it with the metadata the history store already has,
// so a choice is informed rather than a guess.

// historyPickCount bounds what the picker loads — enough to search
// meaningfully, small enough to stay instant.
const historyPickCount = 5000

// historyPickFn returns the editor's HistoryPick hook, or nil when
// there is no history to pick from.
//
// host may be nil (no plugin host, or none installed). When it is not,
// HistoryBackend plugins are asked for matches too (#97) and merged
// behind the local ones — the local store answers first and always, so a
// slow or broken backend costs reach, never the picker.
func historyPickFn(store *history.Store, host *pluginhost.Host) func(string) (string, bool) {
	if store == nil {
		return nil
	}
	return func(query string) (string, bool) {
		local := store.RecentEntries(historyPickCount)
		localSet := commandSet(local)
		entries := mergeHistory(local,
			searchBackends(context.Background(), host, query, currentDir(), historyPickCount, false),
			historyPickCount)
		if len(entries) == 0 {
			return "", false
		}
		previews := outputPreviews(entries)
		items := make([]ui.PickerItem, 0, len(entries))
		for i, e := range entries {
			detail := historyDetail(e)
			if p := previews[i]; p != "" {
				detail = strings.TrimSpace(detail + "  " + p)
			}
			if _, isLocal := localSet[e.Command]; !isLocal {
				// Say where it came from. A command this machine never
				// ran, appearing unlabelled, reads as a bug.
				detail = strings.TrimSpace(remoteMark + " " + detail)
			}
			items = append(items, ui.PickerItem{
				Value:  e.Command,
				Detail: detail,
				Bad:    e.ExitCode != 0,
			})
		}
		selected, ok, usable := ui.Pick(os.Stdin, os.Stdout, items, ui.PickerOptions{
			Prompt: "history",
			Query:  query,
			Height: 15,
		})
		if !usable {
			// No TUI here: the editor's incremental search still works,
			// and returning false leaves the line untouched.
			return "", false
		}
		if !ok || len(selected) == 0 {
			return "", false
		}
		return selected[0], true
	}
}

// historyDetail renders the columns that make a history row worth
// choosing: where it ran, how long ago, and whether it worked.
func historyDetail(e history.Entry) string {
	var parts []string
	if e.Cwd != "" {
		parts = append(parts, displayPath(e.Cwd))
	}
	if e.StartedUnixMs > 0 {
		parts = append(parts, humanAge(time.Since(time.UnixMilli(e.StartedUnixMs))))
	}
	if e.DurationMs >= 1000 {
		parts = append(parts, fmt.Sprintf("took %ds", e.DurationMs/1000))
	}
	if e.ExitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit %d", e.ExitCode))
	}
	return strings.Join(parts, " · ")
}

// humanAge is a compact relative time: the picker has one column for
// it, so "3d" beats "3 days ago".
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

const pickUsage = `usage: … | pick [-m] [--prompt LABEL]

  ls | pick                 choose one line, print it
  ls | pick -m              Tab marks several, Enter prints them all
  git branch | pick --prompt branch

Reads candidates from stdin, writes the selection to stdout — the fzf
primitive, with nothing to install. Without a terminal it is a no-op
pass-through of nothing, so scripts do not hang.`

// pickCallHandler intercepts `pick`, config-style.
func pickCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "pick" {
			return next(ctx, args)
		}
		return runPick(interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runPick(hc interp.HandlerContext, args []string) []string {
	opts := ui.PickerOptions{Prompt: "pick"}
	for len(args) > 0 {
		switch {
		case args[0] == "-m", args[0] == "--multi":
			opts.Multi, args = true, args[1:]
		case args[0] == "--prompt" && len(args) > 1:
			opts.Prompt, args = args[1], args[2:]
		case args[0] == "help", args[0] == "-h", args[0] == "--help":
			fmt.Fprintln(hc.Stdout, pickUsage)
			return []string{"true"}
		default:
			fmt.Fprintf(hc.Stderr, "pick: unknown argument %q\n%s\n", args[0], pickUsage)
			return []string{"false"}
		}
	}

	lines := readLines(hc.Stdin)
	if len(lines) == 0 {
		return []string{"false"} // nothing to choose from
	}
	items := make([]ui.PickerItem, len(lines))
	for i, line := range lines {
		items[i] = ui.PickerItem{Value: line}
	}

	// stdin is the candidate list, so the picker reads keys from the
	// terminal directly — the standard trick every picker uses when it
	// is on the right-hand side of a pipe.
	tty, err := os.Open("/dev/tty")
	if err != nil {
		fmt.Fprintln(hc.Stderr, "pick: no terminal to read keys from")
		return []string{"false"}
	}
	defer tty.Close()

	selected, ok, usable := ui.Pick(tty, hc.Stdout, items, opts)
	if !usable {
		fmt.Fprintln(hc.Stderr, "pick: needs an interactive terminal")
		return []string{"false"}
	}
	if !ok || len(selected) == 0 {
		return []string{"false"} // aborted: no output, nonzero status
	}
	for _, s := range selected {
		fmt.Fprintln(hc.Stdout, s)
	}
	return []string{"true"}
}

// readLines slurps stdin, dropping the trailing blank.
func readLines(r interface{ Read([]byte) (int, error) }) []string {
	if r == nil {
		return nil
	}
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	lines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// currentDir is the cwd a backend may rank by. An unknowable cwd is not
// worth failing over — the backend simply ranks without locality.
func currentDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

// Output previews in ctrl-r (#99, the last of the blocks staged plan).
//
// A history line tells you a command ran and how it exited. What people
// actually search for is what it *printed* — the error, the id, the
// path — and that is the difference between a history list and a block
// list.

// previewCount bounds how many entries get their output read. Ctrl-r has
// to feel instant: the picker builds every row before it paints, so a
// file read per row across the whole history would be paid on every
// keystroke that opens it. The newest entries are the ones anyone
// scrolls to, so the rest simply go without.
const previewCount = 150

// previewBytes is how much of a block is read to find its first
// interesting line. A build log can be hundreds of kilobytes and the
// preview is one line.
const previewBytes = 4 << 10

// outputPreviews returns a preview per entry, parallel to entries and
// empty where there is nothing to show.
//
// Empty is the *common* case and never means breakage: capture is
// opt-in, and even with it on neither stderr nor builtin output is
// captured (docs/blocks.md). A row without a preview is a row whose
// output nobody kept.
func outputPreviews(entries []history.Entry) []string {
	out := make([]string, len(entries))
	if blockStore == nil {
		return out
	}
	for i, e := range entries {
		if i >= previewCount {
			break
		}
		if e.Block == "" {
			continue
		}
		data, ok := blockStore.Get(blocks.Ref(e.Block))
		if !ok {
			continue
		}
		if len(data) > previewBytes {
			data = data[:previewBytes]
		}
		out[i] = firstInterestingLine(string(data))
	}
	return out
}

// firstInterestingLine picks the line worth showing beside a command.
//
// Not simply the first: a command that greets before it works ("Cloning
// into...", a progress bar) would preview its least useful line. A
// failed command's message is what someone is looking for, so a line
// that looks like an error wins; otherwise the first non-blank line
// stands in.
func firstInterestingLine(out string) string {
	var first string
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		line = strings.TrimSpace(ansiEscapes.ReplaceAllString(line, ""))
		if line == "" {
			continue
		}
		if first == "" {
			first = line
		}
		if looksLikeError(line) {
			return truncateCommand(line)
		}
	}
	return truncateCommand(first)
}

// looksLikeError is deliberately crude. It only decides which of a
// command's own lines to show, so a false positive costs a slightly
// worse preview and nothing else.
func looksLikeError(line string) bool {
	lower := strings.ToLower(line)
	for _, marker := range []string{"error", "fatal", "failed", "failure", "panic", "cannot ", "no such "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// ansiEscapes strips color from captured output: the preview is
// rendered dim by the picker, and a stray color code would fight it.
//
// The OSC branch accepts both terminators. BEL alone is not enough:
// hyperlinks are the OSC most likely to appear in a build log, and the
// tools that emit them (cargo, gcc, ls) close with ST. Bounding the
// payload with [^\a\x1b] matters as much as accepting ST — an OSC that
// scanned to a BEL arriving thousands of bytes later would consume the
// interesting part of the log along with it, which is the same
// over-consumption fixed in internal/promptengine's skipEscape.
var ansiEscapes = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\a\x1b]*(?:\a|\x1b\\)|\x1b[=><]`)
