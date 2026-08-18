package repl

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/session"
	"github.com/blairham/koi-shell/internal/ui"
)

// The `sessions` builtin and the restore path (#103).
//
// The shell already knows where it is standing, what it changed about
// its environment, which toolchains are pinned, and what it last ran.
// tmux-resurrect and continuum exist to reconstruct that from outside,
// badly, because no shell has ever offered it.
//
// The rule the whole feature turns on: **landing somewhere is harmless,
// changing your environment is not.** A restore puts the shell in the
// recorded directory immediately, and hands the recorded environment to
// the #12 trust flow as a *proposal*. A file written by a previous
// process never silently changes this one.

const sessionsUsage = `usage: sessions [list|show ID|restore ID|forget ID]

  sessions                list recent sessions, newest first
  sessions show 3f2a      what that session would restore
  sessions restore 3f2a   cd there; its environment is re-proposed
  sessions forget 3f2a    drop the record

Sessions are recorded continuously at each prompt and live in
$XDG_STATE_HOME/koi/sessions. Restoring changes your directory
immediately; the environment is offered through the same trust prompt an
env plugin gets, never applied silently. Secrets are never recorded —
credential-shaped variable names are filtered before anything is
written, so a session file cannot leak one.`

// sessionsCallHandler intercepts `sessions`, config-style.
func sessionsCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "sessions" {
			return next(ctx, args)
		}
		return runSessions(ctx, interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runSessions(ctx context.Context, hc interp.HandlerContext, args []string) []string {
	fail := func(err error) []string {
		fmt.Fprintln(hc.Stderr, "sessions:", err)
		return []string{"false"}
	}
	store, err := session.OpenDefault()
	if err != nil {
		return fail(err)
	}

	switch {
	case len(args) == 0, args[0] == "list":
		return listSessions(hc, store)

	case args[0] == "help", args[0] == "-h", args[0] == "--help":
		fmt.Fprintln(hc.Stdout, sessionsUsage)
		return []string{"true"}

	case args[0] == "show" && len(args) == 2:
		rec, err := store.Get(args[1])
		if err != nil {
			return fail(err)
		}
		showSession(hc, rec)
		return []string{"true"}

	case args[0] == "forget" && len(args) == 2:
		rec, err := store.Get(args[1])
		if err != nil {
			return fail(err)
		}
		if err := store.Remove(rec.ID); err != nil {
			return fail(err)
		}
		fmt.Fprintf(hc.Stdout, "forgot %s\n", session.ShortID(rec.ID))
		return []string{"true"}

	case args[0] == "restore" && len(args) == 2:
		rec, err := store.Get(args[1])
		if err != nil {
			return fail(err)
		}
		return restoreSession(ctx, hc, rec)
	}
	return fail(fmt.Errorf("unknown usage\n%s", sessionsUsage))
}

// listSessions prints the recorded sessions. Styled on a terminal,
// plain everywhere else — the #90 rule.
func listSessions(hc interp.HandlerContext, store *session.Store) []string {
	records := store.List()
	if len(records) == 0 {
		fmt.Fprintln(hc.Stdout, "no sessions recorded yet")
		return []string{"true"}
	}
	style := ui.Styles(ui.Enabled(hc.Stdout))
	now := time.Now()
	for _, r := range records {
		detail := displayPath(r.Cwd)
		if r.LastCommand != "" {
			detail += "  " + style.Dim.Render(truncateCommand(r.LastCommand))
		}
		fmt.Fprintf(hc.Stdout, "%s  %-6s  %s\n",
			style.Bold.Render(session.ShortID(r.ID)),
			humanAge(r.Age(now)),
			detail)
	}
	return []string{"true"}
}

// truncateCommand keeps the listing to one line per session.
func truncateCommand(cmd string) string {
	cmd = strings.ReplaceAll(cmd, "\n", " ")
	if len(cmd) > 60 {
		return cmd[:57] + "…"
	}
	return cmd
}

// showSession explains exactly what a restore would do, before doing
// it. The environment is the part worth previewing: it is the only
// thing a restore proposes rather than performs.
func showSession(hc interp.HandlerContext, r session.Record) {
	style := ui.Styles(ui.Enabled(hc.Stdout))
	fmt.Fprintf(hc.Stdout, "%s  %s\n", style.Bold.Render(session.ShortID(r.ID)), displayPath(r.Cwd))
	if r.LastCommand != "" {
		fmt.Fprintf(hc.Stdout, "  last     %s\n", truncateCommand(r.LastCommand))
	}
	for name, value := range r.Env {
		fmt.Fprintf(hc.Stdout, "  env      %s=%s\n", name, value)
	}
	for tool, version := range r.Pins {
		fmt.Fprintf(hc.Stdout, "  pin      %s %s\n", tool, version)
	}
	for _, job := range r.Jobs {
		fmt.Fprintf(hc.Stdout, "  job      %s\n", truncateCommand(job))
	}
	if len(r.Env) == 0 && len(r.Pins) == 0 && len(r.Jobs) == 0 {
		fmt.Fprintln(hc.Stdout, "  (directory only)")
	}
}

// restoreSession returns the command line to run. The cd is rewritten
// for the interpreter — the same CallHandler trick `z` uses — so the
// directory change happens in the session itself rather than in a
// subshell where it would evaporate.
//
// The environment is deliberately *not* part of that rewrite. It is
// offered to the trust flow, which prompts, because re-applying
// variables from a file written by a dead process is exactly the thing
// #12 exists to gate.
func restoreSession(ctx context.Context, hc interp.HandlerContext, r session.Record) []string {
	if r.Cwd == "" {
		fmt.Fprintln(hc.Stderr, "sessions: that record has no directory")
		return []string{"false"}
	}
	if _, err := os.Stat(r.Cwd); err != nil {
		// The directory is gone — a deleted checkout, an unmounted
		// volume. Say so rather than dropping the user somewhere else.
		fmt.Fprintf(hc.Stderr, "sessions: %s is gone (%v)\n", displayPath(r.Cwd), err)
		return []string{"false"}
	}

	if len(r.Env) > 0 {
		if n := proposeRestoredEnv(ctx, r); n > 0 {
			fmt.Fprintf(hc.Stdout, "%d environment change(s) proposed — `trust` to review, `trust allow` to apply\n", n)
		}
	}
	for _, job := range r.Jobs {
		// Processes do not survive; their intent does. Printing rather
		// than running is the whole point — re-launching someone's
		// background jobs unasked would be a genuinely bad surprise.
		fmt.Fprintf(hc.Stdout, "was running: %s\n", truncateCommand(job))
	}
	return []string{"cd", r.Cwd}
}

// proposeRestoredEnv hands the recorded environment to the trust flow
// and reports how many changes it pended.
func proposeRestoredEnv(_ context.Context, r session.Record) int {
	if envMgr == nil {
		return 0
	}
	return envMgr.pendRestored(r.Cwd, r.Env)
}
