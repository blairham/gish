package repl

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/blocks"
	"github.com/blairham/gish/internal/history"
	"github.com/blairham/gish/internal/ui"
)

// The `blocks` builtin (#99 stage 4): a command and its output as one
// navigable unit.
//
// This is the point of the whole feature. Stage 1 gave terminals the
// marks to navigate by, stage 2 captured the output, stage 3 stored it
// — none of which the user can see. A `blocks` command that listed
// commands without their output would be `history` wearing a new name;
// what makes it worth having is that the output is really there.

const blocksUsage = `usage: blocks [list|show ID|search TERM]

  blocks                 recent commands that have captured output
  blocks show 3          replay that command's output
  blocks search "error"  commands whose output matched

Capture is off by default — ` + "`config blocks on`" + ` turns it on, and only
commands run after that have output to show. Output is redacted for
credentials as it is stored, so a block can be shown safely.`

// blocksCallHandler intercepts `blocks`, config-style.
func blocksCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "blocks" {
			return next(ctx, args)
		}
		return runBlocks(interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runBlocks(hc interp.HandlerContext, args []string) []string {
	store := openHistory()
	if store == nil {
		fmt.Fprintln(hc.Stderr, "blocks: no history store")
		return []string{"false"}
	}
	defer store.Close() //nolint:errcheck // read-only

	bs := blockStore
	if bs == nil { // `gish -c blocks` runs without the session's store
		var err error
		if bs, err = blocks.OpenDefault(); err != nil {
			fmt.Fprintln(hc.Stderr, "blocks:", err)
			return []string{"false"}
		}
	}

	switch {
	case len(args) == 0:
		// Bare `blocks` on a terminal is the picker; everywhere else it
		// is the listing. Same rule as `config theme` and `plugin
		// browse`, and it keeps the command useful in scripts.
		if pick := pickBlock(hc, store, bs); pick != nil {
			return pick
		}
		return listBlocks(hc, store)
	case args[0] == "list":
		// An explicit `list` always prints, so there is a way to get the
		// plain output on a terminal.
		return listBlocks(hc, store)
	case args[0] == "help", args[0] == "-h", args[0] == "--help":
		fmt.Fprintln(hc.Stdout, blocksUsage)
		return []string{"true"}
	case args[0] == "show" && len(args) == 2:
		return showBlock(hc, store, bs, args[1])
	case args[0] == "search" && len(args) == 2:
		return searchBlocks(hc, store, bs, args[1])
	}
	fmt.Fprintf(hc.Stderr, "blocks: unknown usage\n%s\n", blocksUsage)
	return []string{"false"}
}

// withOutput returns the recent entries that actually have output,
// newest first — the only ones worth listing.
func withOutput(store *history.Store) []history.Entry {
	var out []history.Entry
	for _, e := range store.RecentEntries(blockListCount) {
		if e.Block != "" {
			out = append(out, e)
		}
	}
	return out
}

// blockListCount bounds the scan. Blocks retention caps the store far
// below this, so it is a guard rather than a policy.
const blockListCount = 2000

func listBlocks(hc interp.HandlerContext, store *history.Store) []string {
	entries := withOutput(store)
	if len(entries) == 0 {
		fmt.Fprintln(hc.Stdout, "no captured output yet (config blocks on)")
		return []string{"true"}
	}
	style := ui.Styles(ui.Enabled(hc.Stdout))
	now := time.Now()
	for i, e := range entries {
		detail := humanAge(now.Sub(time.UnixMilli(e.StartedUnixMs)))
		if e.ExitCode != 0 {
			detail += fmt.Sprintf(" · exit %d", e.ExitCode)
		}
		cmd := truncateCommand(e.Command)
		if e.ExitCode != 0 {
			cmd = style.Fail.Render(cmd)
		}
		fmt.Fprintf(hc.Stdout, "%s  %s  %s\n",
			style.Bold.Render(fmt.Sprintf("%-3d", i+1)), cmd, style.Dim.Render(detail))
	}
	return []string{"true"}
}

// showBlock replays one block's output. The index is the position in
// the listing, which is what a user just read off the screen.
func showBlock(hc interp.HandlerContext, store *history.Store, bs *blocks.Store, id string) []string {
	entries := withOutput(store)
	idx, err := blockIndex(id, len(entries))
	if err != nil {
		fmt.Fprintln(hc.Stderr, "blocks:", err)
		return []string{"false"}
	}
	e := entries[idx]

	out, ok := bs.Get(blocks.Ref(e.Block))
	if !ok {
		// Derived state: the output aged out or was pruned, and the
		// history entry is still perfectly good.
		fmt.Fprintf(hc.Stderr, "blocks: output for %q is no longer stored\n", truncateCommand(e.Command))
		return []string{"false"}
	}

	style := ui.Styles(ui.Enabled(hc.Stdout))
	fmt.Fprintf(hc.Stdout, "%s %s\n", style.Dim.Render("$"), e.Command)
	if meta, ok := bs.Stat(blocks.Ref(e.Block)); ok {
		var notes []string
		if meta.Truncated {
			notes = append(notes, "output truncated — only the tail was kept")
		}
		if meta.Redacted > 0 {
			notes = append(notes, fmt.Sprintf("%d credential-shaped span(s) redacted", meta.Redacted))
		}
		for _, n := range notes {
			fmt.Fprintln(hc.Stdout, style.Dim.Render("  ("+n+")"))
		}
	}
	_, _ = hc.Stdout.Write(out) //nolint:errcheck // terminal write
	return []string{"true"}
}

// searchBlocks finds commands whose *output* matched — the thing
// history cannot do, and the reason to keep the output at all.
func searchBlocks(hc interp.HandlerContext, store *history.Store, bs *blocks.Store, term string) []string {
	entries := withOutput(store)
	if len(entries) == 0 {
		// "nothing matched" would be true and misleading: it sends the
		// user looking for a better search term when the real answer is
		// that capture was never on.
		fmt.Fprintln(hc.Stdout, "no captured output yet (config blocks on)")
		return []string{"true"}
	}
	style := ui.Styles(ui.Enabled(hc.Stdout))
	matches := 0
	for i, e := range entries {
		out, ok := bs.Get(blocks.Ref(e.Block))
		if !ok || !strings.Contains(string(out), term) {
			continue
		}
		matches++
		fmt.Fprintf(hc.Stdout, "%s  %s\n",
			style.Bold.Render(fmt.Sprintf("%-3d", i+1)), truncateCommand(e.Command))
		if line := firstMatchingLine(string(out), term); line != "" {
			fmt.Fprintf(hc.Stdout, "     %s\n", style.Dim.Render(truncateCommand(line)))
		}
	}
	if matches == 0 {
		fmt.Fprintf(hc.Stdout, "no captured output contains %q\n", term)
	}
	return []string{"true"}
}

// firstMatchingLine gives the search result a line of context, so a hit
// is informative rather than just a command name.
func firstMatchingLine(out, term string) string {
	for line := range strings.Lines(out) {
		if strings.Contains(line, term) {
			return strings.TrimRight(line, "\r\n")
		}
	}
	return ""
}

// blockIndex parses a 1-based listing position.
func blockIndex(id string, n int) (int, error) {
	if n == 0 {
		return 0, fmt.Errorf("no captured output yet (config blocks on)")
	}
	var i int
	if _, err := fmt.Sscanf(id, "%d", &i); err != nil || i < 1 || i > n {
		return 0, fmt.Errorf("%q is not one of the %d listed blocks", id, n)
	}
	return i - 1, nil
}

// pickBlock is the interactive front end (#99 stage 4 over #100): choose
// a command from those that have captured output, and its output is
// shown. Returns nil when there is no terminal to host a picker, so the
// caller falls back to the listing.
func pickBlock(hc interp.HandlerContext, store *history.Store, bs *blocks.Store) []string {
	entries := withOutput(store)
	if len(entries) == 0 {
		return nil // nothing to pick; the listing explains why
	}
	f, isFile := hc.Stdin.(*os.File)
	if !isFile || !ui.Enabled(hc.Stdout) || !ui.Enabled(f) {
		return nil
	}

	now := time.Now()
	items := make([]ui.PickerItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, ui.PickerItem{
			Value:  e.Command,
			Detail: blockDetail(e, now),
			Bad:    e.ExitCode != 0,
		})
	}
	selected, ok, usable := ui.Pick(f, hc.Stdout, items, ui.PickerOptions{
		Prompt: "blocks",
		Height: 15,
	})
	if !usable {
		return nil
	}
	if !ok || len(selected) == 0 {
		return []string{"true"} // aborted: not an error
	}

	// The picker returns the command text, and the same command may have
	// run many times. The most recent is the right answer: someone
	// picking `make build` off a list means the one they just ran, not
	// one from last week. entries is newest-first, so the first match is
	// it.
	for i, e := range entries {
		if e.Command == selected[0] {
			return showBlock(hc, store, bs, fmt.Sprint(i+1))
		}
	}
	return []string{"true"}
}

// blockDetail is the dimmed column beside a command: when it ran, how it
// exited, and how much output there is to show.
func blockDetail(e history.Entry, now time.Time) string {
	parts := []string{humanAge(now.Sub(time.UnixMilli(e.StartedUnixMs)))}
	if e.ExitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit %d", e.ExitCode))
	}
	if e.Cwd != "" {
		parts = append(parts, displayPath(e.Cwd))
	}
	return strings.Join(parts, " · ")
}
