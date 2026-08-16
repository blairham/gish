// Package repl implements gish's read-eval loop on top of mvdan.cc/sh's
// POSIX/bash parser and interpreter.
//
// Interactive terminals get the raw-mode line editor (internal/editor);
// piped stdin falls back to the plain line loop so `echo cmd | gish` and
// tests behave like a non-interactive shell. Script and -c execution are
// separate paths via RunReader.
package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/builtins"
	"github.com/blairham/gish/internal/editor"
	"github.com/blairham/gish/internal/envtrust"
	"github.com/blairham/gish/internal/history"
	"github.com/blairham/gish/internal/jobs"
	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/internal/term"
)

const (
	prompt     = "gish$ "
	contPrompt = "> "
)

// Run starts the interactive loop on stdin and blocks until EOF or exit.
// The returned error is the session's exit status (an interp.ExitStatus)
// when the user ran exit, or a real I/O/parse failure.
func Run(ctx context.Context, login bool) error {
	if term.IsTerminal(os.Stdin) {
		return runEditor(ctx, login)
	}
	return runPlain(ctx, login)
}

// runEditor is the interactive path: the line editor owns the terminal
// between commands; the interpreter owns it while a command runs.
//
// Signal posture (see #3): an interactive shell must never die from the
// user's Ctrl-C or Ctrl-\. At the prompt those arrive as key events (raw
// mode); while a command runs, the terminal delivers them to the whole
// foreground process group — children included, which is what kills the
// child. gish catches its own copy via Notify (NOT Ignore: an ignored
// disposition would be inherited across exec and make children immune to
// Ctrl-C) and reacts by canceling the command context, which is what
// stops pure-builtin loops the kernel can't reach. SIGTSTP is left at
// its default until job control (#5).
func runEditor(ctx context.Context, login bool) error {
	// Native brew shellenv (#44): pure stat/string work, no subprocess.
	brewShellenv()

	// Tier-2 plugin host (#7): discovery now, launch on first demand.
	// Prompt segments are consumed via %p{id} escapes; the `plugins`
	// builtin makes the host inspectable. Plugin-provided commands (#11)
	// dispatch through the command index, after gish builtins and before
	// PATH.
	var segs *segmentRenderer
	var host *pluginhost.Host
	var cmdIndex *pluginhost.CommandIndex
	if dir, derr := pluginhost.DefaultDir(); derr == nil {
		host = pluginhost.NewHost(dir)
		if derr := host.Discover(); derr != nil {
			fmt.Fprintln(os.Stderr, "gish: plugins:", derr)
		}
		defer host.Close()
		cmdIndex = host.NewCommandIndex(reservedCommandName)
		builtins.Register("plugins", pluginsBuiltin(host, cmdIndex, dir))
		segs = newSegmentRenderer(host)
		themePlugins = newThemeRenderer(host)
		// Env diffs (#12): a corrupt trust file disables the feature for
		// the session (doctor explains); it must never reset silently.
		if trustPath, terr := envtrust.DefaultPath(); terr == nil {
			if store, serr := envtrust.Open(trustPath); serr == nil {
				envMgr = newEnvManager(host, store, os.Stderr)
			} else {
				fmt.Fprintln(os.Stderr, "gish: env trust:", serr)
			}
		}
	}

	// Job control (#5): externals of each command line run in their own
	// process group and own the terminal while foreground; jobs/fg/bg
	// are gish builtins reached via the CallHandler rewrite.
	table := jobs.NewTable(os.Stdin)
	execChain := []func(interp.ExecHandlerFunc) interp.ExecHandlerFunc{builtins.ExecHandler}
	if cmdIndex != nil {
		execChain = append(execChain, cmdIndex.ExecMiddleware)
	}
	var runnerRef *interp.Runner
	execChain = append(execChain,
		notFoundMiddleware(func() *interp.Runner { return runnerRef }),
		sandboxExecMiddleware,
		table.ExecMiddleware)
	runnerOpts := []interp.RunnerOption{
		interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
		interp.ExecHandlers(execChain...),
	}
	callBase := passthroughCall
	if jobs.Supported() {
		// Reclaiming the terminal from the background must not stop the
		// shell. Children inherit the ignore; acceptable (see #5 design).
		ignoreTTOU()
		builtins.Register("__gish_jobs", table.Jobs)
		builtins.Register("__gish_fg", table.Fg)
		builtins.Register("__gish_bg", table.Bg)
		callBase = jobs.RewriteCall
	}
	runnerOpts = append(runnerOpts, interp.CallHandler(explainCallHandler(toolCallHandler(sandboxCallHandler(trustCallHandler(doctorCallHandler(configCallHandler(ziCallHandler(callBase)))))))))
	runner, err := interp.New(runnerOpts...)
	if err != nil {
		return err
	}
	runnerRef = runner
	trustRunner = func() *interp.Runner { return runnerRef }

	// History failure degrades, never blocks the shell.
	var hist editor.History
	store := openHistory()
	if store != nil {
		defer store.Close() //nolint:errcheck // exit path; entries are already flushed per-append
		hist = store
	}
	sessionID := fmt.Sprintf("%d-%x", os.Getpid(), time.Now().UnixNano())
	if store != nil {
		store.SetSession(sessionID) // reloads (#40) skip our own entries
	}
	if host != nil {
		aiMgr = newAIManager(host, store)
	}

	edCfg := editor.Config{
		Prompt:     prompt,
		ContPrompt: contPrompt,
		AcceptWhen: acceptWhen,
		History:    hist,
		Complete:   completionFn(runner, host),
	}
	// The fish-parity pair (#38/#39): parser-driven highlighting and
	// history ghost text — skipped where color is unwelcome.
	colorOK := os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	if colorOK {
		edCfg.Highlight = highlightFn(runner)
		if store != nil {
			edCfg.Suggest = func(text string) string {
				if s, ok := store.Match(text, 0); ok {
					return s
				}
				return ""
			}
		}
	}
	// Footgun diagnostics (#46) are content, not decoration: they stay on
	// without color, just unstyled.
	edCfg.Diagnose = lintFn(runner, colorOK)
	ed := editor.New(term.NewTTY(os.Stdin, os.Stdout), os.Stdout, edCfg)
	parser := syntax.NewParser()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(sigs)

	// Login shells source profile files first (#41), then the rc file
	// runs in the session runner so functions, vars, and cd persist.
	if login {
		loadProfile(ctx, runner)
	}
	loadRC(ctx, runner)
	starshipHint(shellVar(runner, "GISH_THEME", "") != "", shellVar(runner, "GISH_PROMPT", "") != "")
	info := newPromptInfo()
	lastExit := 0

	toolsMgr = newToolsManager(os.Stderr)
	lastDuration := time.Duration(0)
	for {
		// Native tool-version switching (#77) rebuilds PATH first —
		// first-party env work precedes EnvProvider plugins (#12), which
		// revert/propose/apply on dir change after it.
		toolsMgr.atPrompt(ctx, runner)
		if envMgr != nil {
			envMgr.atPrompt(ctx, runner)
		}
		info.dir = runner.Dir
		info.exitCode = lastExit
		info.duration = lastDuration
		info.jobs = table.Count()
		if w, _, werr := term.NewTTY(os.Stdin, os.Stdout).Size(); werr == nil {
			info.width = w
		}
		if segs != nil {
			info.segment = func(id string) string {
				return segs.render(ctx, id, runner.Dir, lastExit, func(keys []string) map[string]string {
					return requestedEnv(runner, keys)
				})
			}
		}
		p, cp, rp := promptStrings(runner, info)
		ed.SetPrompt(p, cp)
		ed.SetRPrompt(rp)

		line, err := ed.ReadCommand(ctx)
		switch {
		case errors.Is(err, editor.ErrInterrupted):
			continue // Ctrl-C: fresh prompt
		case errors.Is(err, io.EOF):
			return nil // Ctrl-D on an empty line
		case err != nil:
			return err
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		// The agent surface (#34): plan first, approve, execute gated
		// steps through the real exec path — intercepted before parsing,
		// like ??, so the orchestration owns the whole interaction.
		if task, isAgent := strings.CutPrefix(strings.TrimSpace(line), "agent "); isAgent || strings.TrimSpace(line) == "agent" {
			drainSignals(sigs)
			agentCtx, cancelAgent := context.WithCancel(ctx)
			go func() {
				select {
				case <-sigs:
					cancelAgent()
				case <-agentCtx.Done():
				}
			}()
			execStep := func(stepLine string) int {
				file, perr := parser.Parse(strings.NewReader(stepLine), "agent-step")
				if perr != nil {
					fmt.Fprintln(os.Stderr, "gish:", perr)
					return 2
				}
				drainSignals(sigs)
				table.BeginLine(stepLine)
				rerr := runInterruptible(agentCtx, runner, file, sigs)
				if n, ok := table.EndLine(); ok && n.Stopped {
					fmt.Printf("[%d]  Stopped  %s\n", n.ID, n.Command)
				}
				return exitCode(rerr)
			}
			handleAgent(agentCtx, agentDeps{runner: runner, in: os.Stdin, out: os.Stdout, exec: execStep}, task)
			cancelAgent()
			continue
		}
		// The ?? compose prefix (#20): the query goes to the AI provider
		// and the candidate lands in the next buffer for review — it is
		// never executed here. Ctrl-C cancels the model call.
		if query, isCompose := strings.CutPrefix(strings.TrimSpace(line), "??"); isCompose {
			drainSignals(sigs)
			composeCtx, cancelCompose := context.WithCancel(ctx)
			go func() {
				select {
				case <-sigs:
					cancelCompose()
				case <-composeCtx.Done():
				}
			}()
			handleCompose(composeCtx, runner, strings.TrimSpace(query), ed.Preload, os.Stderr)
			cancelCompose()
			continue
		}
		file, perr := parser.Parse(strings.NewReader(line), "gish")
		if perr != nil {
			fmt.Fprintln(os.Stderr, "gish:", perr)
			continue
		}
		// Multi-line buffers get a shellcheck pass on Enter when it's
		// installed (#46) — off the keystroke path, budget-bounded, and
		// purely advisory: the command runs regardless.
		if lintMode(runner) == "on" && strings.Contains(line, "\n") {
			for _, w := range shellcheckWarnings(ctx, line) {
				fmt.Fprintln(os.Stderr, w)
			}
		}

		drainSignals(sigs) // a signal from prompt-time must not cancel this command
		start := time.Now()
		table.BeginLine(line)
		rerr := runInterruptible(ctx, runner, file, sigs)
		if n, ok := table.EndLine(); ok && n.Stopped {
			fmt.Printf("[%d]  Stopped  %s\n", n.ID, n.Command)
		}
		lastExit = exitCode(rerr)
		lastDuration = time.Since(start)
		if aiMgr != nil {
			aiMgr.note(lastExit)
		}
		if store != nil {
			// Cwd comes from the runner: `cd` moves the interpreter's
			// directory, not the gish process's.
			entry := history.Entry{
				Command:       line,
				StartedUnixMs: start.UnixMilli(),
				DurationMs:    time.Since(start).Milliseconds(),
				ExitCode:      lastExit,
				Cwd:           runner.Dir,
				SessionID:     sessionID,
			}
			skip, aerr := store.Append(entry)
			switch {
			case aerr != nil:
				fmt.Fprintln(os.Stderr, "gish: history:", aerr)
			case skip == history.SkipSecret:
				fmt.Fprintln(os.Stderr, "gish: history: possible secret detected — command not recorded")
			case skip == history.SkipNone:
				// Backends only ever see entries that passed the scrub.
				fanoutHistory(host, entry)
			}
		}
		switch {
		case errors.Is(rerr, errInterrupted):
			// The command was interrupted, not the shell: fresh prompt.
			// Order matters — Runner.Exited() also reports true after a
			// cancellation, and the runner stays usable with state intact.
		case runner.Exited():
			// `exit` returns a nil error for status 0, so this must be
			// checked before the nil case or the shell can't exit cleanly.
			return rerr
		case rerr == nil:
		default:
			if _, ok := errors.AsType[interp.ExitStatus](rerr); !ok {
				fmt.Fprintln(os.Stderr, "gish:", rerr)
			}
		}
	}
}

// errInterrupted marks a command run that ended because the user
// interrupted it — the shell continues, silently.
var errInterrupted = errors.New("command interrupted")

// runInterruptible runs one parsed command, canceling its context when
// SIGINT arrives so builtin-only loops stop too. External children get
// their signal directly from the terminal (same process group — that
// changes with job control, #5). SIGQUIT is swallowed for the shell and
// left to the kernel for the child.
func runInterruptible(ctx context.Context, runner *interp.Runner, file *syntax.File, sigs <-chan os.Signal) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-sigs:
				if sig == os.Interrupt {
					cancel()
				}
			case <-done:
				return
			}
		}
	}()
	err := runner.Run(runCtx, file)
	if err != nil && runCtx.Err() != nil && ctx.Err() == nil {
		return errInterrupted
	}
	return err
}

// drainSignals discards any signal that arrived while no command was
// running.
func drainSignals(sigs <-chan os.Signal) {
	select {
	case <-sigs:
	default:
	}
}

// reservedCommandName reports names a plugin command may not claim:
// interpreter builtins and gish-native builtins (#11 precedence rules).
func reservedCommandName(name string) bool {
	if interp.IsBuiltin(name) || name == "zi" || name == "config" || name == "builtins" || name == "plugins" {
		return true
	}
	return slices.Contains(builtins.Native(), name)
}

// pluginsBuiltin lists discovered tier-2 plugins with their live status,
// capabilities, and registered commands; it launches and Describes
// plugins on demand.
func pluginsBuiltin(host *pluginhost.Host, cmdIndex *pluginhost.CommandIndex, dir string) builtins.Func {
	return func(ctx context.Context, hc interp.HandlerContext, _ []string) error {
		_ = host.Discover() //nolint:errcheck // newly installed plugins picked up best-effort
		statuses := host.Statuses(ctx, true)
		if len(statuses) == 0 {
			fmt.Fprintf(hc.Stdout, "no plugins installed — drop executables in %s\n", dir)
			return nil
		}
		printPluginStatuses(hc.Stdout, statuses, cmdIndex)
		return nil
	}
}

// openHistory opens the default history store; on failure the shell
// runs without history rather than refusing to start.
func openHistory() *history.Store {
	path, err := history.DefaultPath()
	if err == nil {
		var store *history.Store
		if store, err = history.Open(path); err == nil {
			return store
		}
	}
	fmt.Fprintln(os.Stderr, "gish: history disabled:", err)
	return nil
}

// exitCode maps a command's error to the status recorded in history.
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errInterrupted):
		return 130 // 128+SIGINT, the shell convention
	default:
		if status, ok := errors.AsType[interp.ExitStatus](err); ok {
			return int(status)
		}
		return 1
	}
}

// acceptWhen reports whether text parses as a complete program. Syntax
// errors submit too — the interpreter reports them, matching how a shell
// treats a finished-but-wrong line.
func acceptWhen(text string) bool {
	_, err := syntax.NewParser().Parse(strings.NewReader(text), "gish")
	return err == nil || !syntax.IsIncomplete(err)
}

// runPlain is the non-TTY loop (piped stdin).
func runPlain(ctx context.Context, login bool) error {
	runner, err := interp.New(
		interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
		interp.ExecHandlers(builtins.ExecHandler, sandboxExecMiddleware),
		interp.CallHandler(explainCallHandler(toolCallHandler(sandboxCallHandler(trustCallHandler(doctorCallHandler(configCallHandler(ziCallHandler(passthroughCall)))))))),
	)
	if err != nil {
		return err
	}
	if login {
		loadProfile(ctx, runner)
	}
	parser := syntax.NewParser()

	var exitErr error
	fmt.Fprint(os.Stdout, prompt)
loop:
	for stmts, err := range parser.InteractiveSeq(os.Stdin) {
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if parser.Incomplete() {
			fmt.Fprint(os.Stdout, contPrompt)
			continue
		}
		for _, stmt := range stmts {
			if err := runner.Run(ctx, stmt); err != nil {
				if runner.Exited() {
					exitErr = err
					break loop
				}
				// Nonzero statuses are ordinary interactive life; only
				// surface real interpreter errors.
				if _, ok := errors.AsType[interp.ExitStatus](err); !ok {
					fmt.Fprintln(os.Stderr, "gish:", err)
				}
			}
		}
		fmt.Fprint(os.Stdout, prompt)
	}
	fmt.Fprintln(os.Stdout)
	return exitErr
}

// RunCommand parses and runs src as a complete script (gish -c). This
// is the path tools take when they spawn $SHELL -c: it stays POSIX-clean
// — no editor, theme, plugins, history, or extra output (#41).
func RunCommand(ctx context.Context, src string, login bool) error {
	return runScript(ctx, strings.NewReader(src), "gish -c", login)
}

// RunFile runs the script at path.
func RunFile(ctx context.Context, path string, login bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return runScript(ctx, f, path, login)
}

// runScript is the non-interactive execution path, optionally preceded
// by login profile sourcing.
func runScript(ctx context.Context, r io.Reader, name string, login bool) error {
	file, err := syntax.NewParser().Parse(r, name)
	if err != nil {
		return err
	}
	runner, err := interp.New(
		interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
		interp.ExecHandlers(builtins.ExecHandler, sandboxExecMiddleware),
		interp.CallHandler(explainCallHandler(toolCallHandler(sandboxCallHandler(trustCallHandler(doctorCallHandler(configCallHandler(ziCallHandler(passthroughCall)))))))),
	)
	if err != nil {
		return err
	}
	if login {
		loadProfile(ctx, runner)
	}
	return runner.Run(ctx, file)
}

// RunReader parses and runs an entire script from r; name appears in
// error messages. Later opts override the default stdio, which keeps the
// core loop testable without touching the real terminal.
func RunReader(ctx context.Context, r io.Reader, name string, opts ...interp.RunnerOption) error {
	file, err := syntax.NewParser().Parse(r, name)
	if err != nil {
		return err
	}
	runner, err := interp.New(append(
		[]interp.RunnerOption{
			interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
			interp.ExecHandlers(builtins.ExecHandler, sandboxExecMiddleware),
			interp.CallHandler(explainCallHandler(toolCallHandler(sandboxCallHandler(trustCallHandler(doctorCallHandler(configCallHandler(ziCallHandler(passthroughCall)))))))),
		},
		opts...,
	)...)
	if err != nil {
		return err
	}
	return runner.Run(ctx, file)
}
