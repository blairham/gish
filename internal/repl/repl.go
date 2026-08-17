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
	resetBashHooks()
	resetCompletions()

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
	// Keep a live capture pty the same size as the real terminal. The
	// editor's own resize handling only runs while it is reading, and a
	// command that resizes its window mid-run still needs its stdout to
	// report the truth.
	blockStore = openBlockStore()
	defer pruneBlocks()
	stopWinch := watchResize(func() {
		if w, h, err := term.NewTTY(os.Stdin, os.Stdout).Size(); err == nil {
			table.Resize(w, h)
		}
		// A WINCH trap is noted here and run at the next prompt: this
		// callback is another goroutine, and a runner is not safe to
		// enter from two at once.
		noteSignal("WINCH")
	})
	defer stopWinch()
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
		interp.Env(sessionEnv(true)),
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
	runnerOpts = append(runnerOpts, interp.CallHandler(printfCallHandler(migrateCallHandler(evalSeparatorCallHandler(completeCallHandler(bindCallHandler(trapCallHandler(shoptCallHandler(setOptionCallHandler(blocksCallHandler(clipCallHandler(sessionsCallHandler(pickCallHandler(pluginCallHandler(lazyCallHandler(zCallHandler(explainCallHandler(toolCallHandler(p10kCallHandler(sandboxCallHandler(trustCallHandler(doctorCallHandler(configCallHandler(ziCallHandler(callBase)))))))))))))))))))))))))
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
	// Native z (#94): the loop is the tracking point; the index saves
	// on exit and bootstraps from history on first run.
	jumpMgr = newJumpManager(store)
	if jumpMgr != nil {
		defer jumpMgr.save()
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
	//
	// Both are per-feature knobs rather than NO_COLOR's all-or-nothing
	// (#163), and both read the setting per call so an rc — which is
	// sourced further down — still governs them.
	colorOK := os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	if colorOK {
		edCfg.Highlight = highlightFn(runner)
		if store != nil {
			edCfg.Suggest = func(text string) string {
				if !suggestEnabled(runner) {
					return ""
				}
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
	// Ctrl-X Ctrl-E: edit the line in $EDITOR (#96).
	edCfg.ExternalEdit = externalEditFn(runner)
	// Ctrl-R is the full-screen fuzzy picker when the terminal can host
	// it (#100); incremental search remains the fallback.
	edCfg.HistoryPick = historyPickFn(store, host)
	// `bind -x` commands run with the terminal ceded, so a full-screen
	// widget (fzf's picker) really owns stdin (#159).
	edCfg.KeyCommand = keyCommandRunner(runner)
	ed := editor.New(term.NewTTY(os.Stdin, os.Stdout), os.Stdout, edCfg)
	editorRef = func() *editor.Editor { return ed }
	parser := syntax.NewParser()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(sigs)

	// Alias expansion goes on before anything is sourced, so aliases
	// defined in a profile or rc work in the session they configure.
	enableAliases(ctx, runner)
	// What the shell answers to a feature probe (#120): set before any
	// init script runs, since that is what those scripts branch on.
	declareShellIdentity(ctx, runner)
	// Login shells source profile files first (#41), then the rc file
	// runs in the session runner so functions, vars, and cd persist.
	if login {
		loadProfile(ctx, runner)
	}
	loadRC(ctx, runner)
	// The declarative manifest (#108) loads after the rc, so an rc can
	// still set variables plugins read. A broken manifest is reported,
	// never silently skipped — the user would just see nothing load.
	loadPluginManifest(ctx, runner)
	starshipHint(shellVar(runner, "GISH_THEME", "") != "", shellVar(runner, "GISH_PROMPT", "") != "")
	info := newPromptInfo()
	lastExit := 0

	toolsMgr = newToolsManager(os.Stderr)
	// Session recording (#103): the prompt is both when the state is
	// meaningful and when the shell is idle.
	sessionMgr = newSessionRecorder(sessionID, runner, table.Commands)
	defer sessionMgr.close()
	if id := os.Getenv("GISH_RESTORE_SESSION"); id != "" {
		os.Unsetenv("GISH_RESTORE_SESSION") //nolint:errcheck // consume it once
		if detail, ok := restoreOnStart(id, runner); ok {
			fmt.Fprintf(os.Stderr, "gish: restored %s\n", detail)
		} else {
			fmt.Fprintf(os.Stderr, "gish: --restore: %s\n", detail)
		}
	}
	lastDuration := time.Duration(0)
	lastCommandText := "" // what session recording (#103) reports as "last"
	for {
		// Native tool-version switching (#77) rebuilds PATH first —
		// first-party env work precedes EnvProvider plugins (#12), which
		// revert/propose/apply on dir change after it.
		toolsMgr.atPrompt(ctx, runner)
		if envMgr != nil {
			envMgr.atPrompt(ctx, runner)
		}
		if jumpMgr != nil {
			jumpMgr.note(runner)
		}
		sessionMgr.atPrompt(runner, lastCommandText)
		// Output capture (#99 stage 2) is opt-in and re-read each
		// prompt, so `config blocks on` takes effect without a restart.
		if shellVar(runner, "GISH_BLOCKS", "off") == "on" {
			table.EnableCapture(0)
		} else {
			table.DisableCapture()
		}
		// Signal traps the loop delivers (#159): a WINCH noted while a
		// command ran, and an INT from the command that was just
		// interrupted, both fire here, where the runner is ours.
		runPendingSignalTraps(ctx, runner)
		// The bash hook surface (#159): PROMPT_COMMAND runs before the
		// prompt is built, so a hook that sets PS1, exports variables or
		// records the directory is reflected in the prompt it precedes.
		runPromptCommand(ctx, runner)
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
		// OSC 133 (#99): mark the prompt so terminals can navigate
		// blocks. Zero-width, so the renderer is unaffected.
		feats := semanticFeatures(runner)
		marks := feats.marks
		p = markPrompt(p, marks)
		// OSC 7 goes out when the directory changes — which is what a
		// new tab or split reads to open where you are (#165).
		if info.dir != lastPromptDir {
			markCwd(os.Stdout, feats.cwd, info.dir)
		}
		ed.SetPrompt(p, cp)
		ed.SetRPrompt(rp)
		// The short prompt the accepted line is left with (#p10k
		// TRANSIENT_PROMPT). Resolved before the read so the editor can
		// swap it in without asking anything at accept time.
		ed.SetTransientPrompt(transientPrompt(runner, info))
		// Vi mode is read per prompt, so an rc setting — or a live
		// `config editmode vi` — takes effect on the next line rather
		// than the next shell (#163).
		ed.SetEditMode(editModeOf(runner))
		lastPromptDir = info.dir

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
		// History expansion (#96): !!, !$, !^, !:N, !prefix, ^old^new.
		// bash echoes the expansion so the user sees what runs.
		if store != nil {
			expanded, changed, herr := expandHistory(line, store.Match)
			switch {
			case herr != nil:
				fmt.Fprintln(os.Stderr, "gish:", herr)
				lastExit = 1
				continue
			case changed:
				fmt.Fprintln(os.Stdout, expanded)
				line = expanded
			}
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
			handleAgent(agentCtx, agentDeps{
				runner: runner, in: os.Stdin, out: os.Stdout, exec: execStep,
				choose: interactiveChooser(os.Stdin, os.Stdout),
			}, task)
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
		markOutputStart(os.Stdout, marks)
		// PS0 prints between the line and its output; the DEBUG trap is
		// the preexec hook, and under extdebug a non-zero return from it
		// cancels the command (#159).
		runPS0(ctx, runner, os.Stdout)
		if !runDebugTrap(ctx, runner, line) {
			lastExit = 1
			markCommandDone(os.Stdout, marks, lastExit)
			continue
		}
		start := time.Now()
		table.BeginLine(line)
		lastCommandText = line
		rerr := runInterruptible(ctx, runner, file, sigs)
		if n, ok := table.EndLine(); ok && n.Stopped {
			fmt.Printf("[%d]  Stopped  %s\n", n.ID, n.Command)
		}
		lastExit = exitCode(rerr)
		markCommandDone(os.Stdout, marks, lastExit)
		lastDuration = time.Since(start)
		markUserVars(os.Stdout, feats.userVars, line, lastDuration)
		if aiMgr != nil {
			aiMgr.note(lastExit)
		}
		if store != nil {
			// The captured output is stored first so the entry can carry
			// its reference, but a failure to store it yields an empty
			// ref rather than an error — the entry is the authoritative
			// record and is written either way (#99 stage 3).
			captured, truncated := table.LastCapture()
			// Cwd comes from the runner: `cd` moves the interpreter's
			// directory, not the gish process's.
			entry := history.Entry{
				Command:       line,
				StartedUnixMs: start.UnixMilli(),
				DurationMs:    time.Since(start).Milliseconds(),
				ExitCode:      lastExit,
				Cwd:           runner.Dir,
				SessionID:     sessionID,
				Block:         recordBlock(captured, truncated),
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
			noteSignal("INT") // the trap fires at the next prompt
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
	var runnerRef *interp.Runner
	trustRunner = func() *interp.Runner { return runnerRef }
	runner, err := interp.New(
		interp.Env(sessionEnv(false)),
		interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
		interp.ExecHandlers(builtins.ExecHandler, sandboxExecMiddleware),
		interp.CallHandler(printfCallHandler(migrateCallHandler(evalSeparatorCallHandler(completeCallHandler(bindCallHandler(trapCallHandler(shoptCallHandler(setOptionCallHandler(blocksCallHandler(clipCallHandler(sessionsCallHandler(pickCallHandler(pluginCallHandler(lazyCallHandler(zCallHandler(explainCallHandler(toolCallHandler(p10kCallHandler(sandboxCallHandler(trustCallHandler(doctorCallHandler(configCallHandler(ziCallHandler(passthroughCall)))))))))))))))))))))))),
	)
	if err != nil {
		return err
	}
	runnerRef = runner
	declareShellIdentity(ctx, runner)
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
func RunCommand(ctx context.Context, src string, login bool, args ...string) error {
	// bash's `-c` takes positional parameters after the command: the
	// first becomes $0 and the rest $1, $2, … It is how every wrapper
	// passes a value into a snippet without interpolating it —
	// `gish -c 'rm -- "$1"' _ "$file"` is the safe spelling, and
	// dropping the parameters turns it into `rm --`.
	name := "gish -c"
	var params []string
	if len(args) > 0 {
		name, params = args[0], args[1:]
	}
	return runScript(ctx, strings.NewReader(src), name, login, params...)
}

// RunFile runs the script at path, with args as its positional
// parameters ($1, $2, …) — a script that cannot be passed arguments is
// not a script.
func RunFile(ctx context.Context, path string, login bool, args ...string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return runScript(ctx, f, path, login, args...)
}

// runScript is the non-interactive execution path, optionally preceded
// by login profile sourcing.
func runScript(ctx context.Context, r io.Reader, name string, login bool, params ...string) error {
	file, err := syntax.NewParser().Parse(r, name)
	if err != nil {
		return err
	}
	var runnerRef *interp.Runner
	trustRunner = func() *interp.Runner { return runnerRef }
	runner, err := interp.New(
		interp.Params(params...),
		interp.Env(sessionEnv(false)),
		interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
		interp.ExecHandlers(builtins.ExecHandler, sandboxExecMiddleware),
		interp.CallHandler(printfCallHandler(migrateCallHandler(evalSeparatorCallHandler(completeCallHandler(bindCallHandler(trapCallHandler(shoptCallHandler(setOptionCallHandler(blocksCallHandler(clipCallHandler(sessionsCallHandler(pickCallHandler(pluginCallHandler(lazyCallHandler(zCallHandler(explainCallHandler(toolCallHandler(p10kCallHandler(sandboxCallHandler(trustCallHandler(doctorCallHandler(configCallHandler(ziCallHandler(passthroughCall)))))))))))))))))))))))),
	)
	if err != nil {
		return err
	}
	runnerRef = runner
	declareShellIdentity(ctx, runner)
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
			interp.CallHandler(printfCallHandler(migrateCallHandler(evalSeparatorCallHandler(completeCallHandler(bindCallHandler(trapCallHandler(shoptCallHandler(setOptionCallHandler(blocksCallHandler(clipCallHandler(sessionsCallHandler(pickCallHandler(pluginCallHandler(lazyCallHandler(zCallHandler(explainCallHandler(toolCallHandler(p10kCallHandler(sandboxCallHandler(trustCallHandler(doctorCallHandler(configCallHandler(ziCallHandler(passthroughCall)))))))))))))))))))))))),
		},
		opts...,
	)...)
	if err != nil {
		return err
	}
	return runner.Run(ctx, file)
}
