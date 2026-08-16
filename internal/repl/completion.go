package repl

import (
	"context"
	"os"
	"strings"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/builtins"
	"github.com/blairham/gish/internal/complete"
	"github.com/blairham/gish/internal/editor"
	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/pkg/pluginapi"
)

// completionFn builds the editor's Tab hook: core command/file
// candidates (docs/plugins.md: core, pure-local) plus plugin providers
// merged behind the 80ms budget. Candidates are raw values — the editor
// escapes on insertion.
func completionFn(runner *interp.Runner, host *pluginhost.Host) func(string, int) editor.CompleteResult {
	return func(text string, cursor int) editor.CompleteResult {
		word, start, isCmd := complete.Analyze(text, cursor)

		// Tab expands $VAR and ~ under the cursor (#96) — the zsh
		// behavior fish converts document missing. A word that names a
		// set variable becomes its value; the single candidate inserts
		// as the expansion.
		if expanded, ok := expandWord(runner, word); ok {
			return editor.CompleteResult{
				WordStart:  start,
				Candidates: []editor.Candidate{{Value: expanded, Display: expanded}},
			}
		}

		var core []complete.Candidate
		if isCmd && !strings.ContainsAny(word, "/~") {
			extra := builtins.ShellBuiltins()
			extra = append(extra, builtins.Native()...)
			extra = append(extra, "zi", "config")
			for name := range runner.Funcs {
				extra = append(extra, name)
			}
			core = complete.Commands(word, pathVar(runner), extra)
		} else {
			core = complete.Files(word, runner.Dir)
		}

		res := editor.CompleteResult{WordStart: start}
		seen := map[string]bool{}
		for _, c := range core {
			seen[c.Value] = true
			res.Candidates = append(res.Candidates, editor.Candidate{Value: c.Value, Display: c.Display})
		}
		for _, c := range pluginCandidates(host, text, cursor, runner.Dir) {
			if !seen[c.Value] {
				seen[c.Value] = true
				res.Candidates = append(res.Candidates, c)
			}
		}
		return res
	}
}

// expandWord resolves $VAR/${VAR} and ~ words to their values;
// ok=false leaves completion to the normal providers.
func expandWord(runner *interp.Runner, word string) (string, bool) {
	switch {
	case strings.HasPrefix(word, "${") && strings.HasSuffix(word, "}") && len(word) > 3:
		name := word[2 : len(word)-1]
		if v, ok := runner.Vars[name]; ok && v.IsSet() {
			return v.String(), true
		}
	case strings.HasPrefix(word, "$") && len(word) > 1:
		name := word[1:]
		if v, ok := runner.Vars[name]; ok && v.IsSet() {
			return v.String(), true
		}
	case word == "~":
		if home, err := os.UserHomeDir(); err == nil {
			return home, true
		}
	}
	return "", false
}

// pathVar resolves PATH: session modifications first, environment after.
func pathVar(runner *interp.Runner) string {
	if v, ok := runner.Vars["PATH"]; ok {
		if s := v.String(); s != "" {
			return s
		}
	}
	return runner.Env.Get("PATH").String()
}

// pluginCandidates asks every CompletionProvider for its first batches
// within the shared budget. A slow provider contributes nothing — Tab
// never waits past the budget.
func pluginCandidates(host *pluginhost.Host, text string, cursor int, cwd string) []editor.Candidate {
	if host == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginhost.DefaultCompleteBudget)
	defer cancel()

	var out []editor.Candidate
	for _, prov := range host.CompletionProviders(ctx) {
		stream, err := prov.Client.Complete(ctx, &pluginapi.CompleteRequest{
			Line:          text,
			Cursor:        uint32(cursor), //nolint:gosec // cursor is a small rune index
			Cwd:           cwd,
			MaxCandidates: 100,
		})
		if err != nil {
			continue
		}
		for {
			batch, err := stream.Recv()
			if err != nil {
				break
			}
			for _, c := range batch.GetCandidates() {
				display := c.GetDisplay()
				if display == "" {
					display = c.GetValue()
				}
				out = append(out, editor.Candidate{Value: c.GetValue(), Display: display})
			}
			if batch.GetFinal() {
				break
			}
		}
	}
	return out
}
