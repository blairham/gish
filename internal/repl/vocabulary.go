package repl

import (
	"context"
	"slices"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/koi-shell/internal/builtins"
	"github.com/blairham/koi-shell/internal/complete"
	"github.com/blairham/koi-shell/internal/pluginhost"
)

// The session command vocabulary (#193): every surface that judges a
// command name — the highlighter's red/green verdict, Tab completion,
// the did-you-mean suggester — must draw on the same answer to "what can
// this session run", or one of them calls a valid command a typo. The
// highlighter shipped with its own private answer (interpreter builtins,
// a hardcoded zi/config pair, functions, PATH) and painted every alias,
// koi command, and plugin command red — the exact signal #38 exists to
// make trustworthy, lying about the commands koi itself documents.

// sessionAliases mirrors the interactive session's alias names. The
// interpreter owns the real definitions and does not export them, so the
// CallHandler observes alias/unalias on their way to the builtin — the
// same post-expansion words the interpreter itself acts on.
var sessionAliases = &aliasNames{}

type aliasNames struct {
	mu  sync.Mutex
	set map[string]bool
}

// observe watches one command call for alias definitions. Only the
// defining form records (`alias ll='ls -la'`); a bare `alias ll` is a
// query and proves nothing about whether the name exists.
func (a *aliasNames) observe(args []string) {
	if len(args) < 2 {
		return
	}
	switch args[0] {
	case "alias":
		a.mu.Lock()
		for _, arg := range args[1:] {
			if len(arg) > 0 && arg[0] == '-' {
				continue // -p prints; a dash never starts an alias name
			}
			if name, _, ok := strings.Cut(arg, "="); ok && name != "" {
				if a.set == nil {
					a.set = map[string]bool{}
				}
				a.set[name] = true
			}
		}
		a.mu.Unlock()
	case "unalias":
		a.mu.Lock()
		for _, arg := range args[1:] {
			switch {
			case arg == "-a":
				a.set = nil
			case len(arg) > 0 && arg[0] != '-':
				delete(a.set, arg)
			}
		}
		a.mu.Unlock()
	}
}

func (a *aliasNames) names() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	names := make([]string, 0, len(a.set))
	for name := range a.set {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// reset clears the mirror; interactive startup calls it so a test that
// ran an earlier session in-process cannot leak names into this one.
func (a *aliasNames) reset() {
	a.mu.Lock()
	a.set = nil
	a.mu.Unlock()
}

// aliasTrackCallHandler keeps the mirror current. It observes and never
// rewrites, and it sits outermost in the interactive chain so it sees
// the words as typed, before any rewrite below it.
func aliasTrackCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		sessionAliases.observe(args)
		return next(ctx, args)
	}
}

// commandIndex is the interactive session's plugin command index, set at
// startup beside pluginMgr: a warm index knows plugin-provided commands
// without launching anything, which is exactly what name-judging
// surfaces need.
var commandIndex *pluginhost.CommandIndex

// sessionCommandNames returns every command name the session answers
// besides PATH executables: implemented shell builtins, koi-native
// builtins, the CallHandler-routed commands, shell functions, aliases,
// pending lazy plugin triggers, and plugin-provided commands.
func sessionCommandNames(runner *interp.Runner) []string {
	extra := builtins.ShellBuiltins()
	extra = append(extra, builtins.Native()...)
	extra = append(extra, callHandlerCommands...)
	for name := range runner.Funcs {
		extra = append(extra, name)
	}
	extra = append(extra, sessionAliases.names()...)
	if pluginMgr != nil {
		extra = append(extra, pluginMgr.triggers()...)
	}
	if commandIndex != nil {
		extra = append(extra, commandIndex.Names()...)
	}
	return extra
}

// knownCommand is the vocabulary as a verdict: does this name resolve if
// run right now? interp.IsBuiltin comes first because it is wider than
// the implemented list — a recognized-but-unsupported name (`newgrp`)
// still answers rather than falling through to command-not-found.
func knownCommand(runner *interp.Runner, name string) bool {
	if interp.IsBuiltin(name) || slices.Contains(sessionCommandNames(runner), name) {
		return true
	}
	return complete.IsCommand(name, pathVar(runner))
}
