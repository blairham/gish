package repl

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/history"
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
func historyPickFn(store *history.Store) func(string) (string, bool) {
	if store == nil {
		return nil
	}
	return func(query string) (string, bool) {
		entries := store.RecentEntries(historyPickCount)
		if len(entries) == 0 {
			return "", false
		}
		items := make([]ui.PickerItem, 0, len(entries))
		for _, e := range entries {
			items = append(items, ui.PickerItem{
				Value:  e.Command,
				Detail: historyDetail(e),
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
