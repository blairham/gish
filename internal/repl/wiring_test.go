package repl

import (
	"path/filepath"
	"testing"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/koi-shell/internal/editor"
	"github.com/blairham/koi-shell/internal/history"
)

// The hooks the editor is handed at startup, tested through the real
// producers rather than the fakes the editor's own tests inject (#201).
//
// Each function here had the shape that hid #193 for months: the
// feature's behavior was covered by unit tests driving an injected
// stand-in, while the code that actually runs in a session was reached
// by no test at all. Two of them (transientPrompt, editModeOf) had
// exactly two references in the whole repo — the definition and the
// call site.

func storeWith(t *testing.T, commands ...string) *history.Store {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, c := range commands {
		skip, err := store.Append(history.Entry{Command: c})
		if err != nil {
			t.Fatal(err)
		}
		if skip != history.SkipNone {
			t.Fatalf("test fixture %q was skipped as %v; it must reach the store", c, skip)
		}
	}
	return store
}

// suggestFn is the ghost-text hook. Before it was extracted it was an
// anonymous closure at the wiring site, so neither half of it — the
// knob or the store lookup — was reachable from a test in the
// combination the shell actually uses.
func TestSuggestFnUsesKnobAndStore(t *testing.T) {
	store := storeWith(t, "make lint", "make test && make lint")

	runner := runnerWithVars(t, nil)
	suggest := suggestFn(runner, store)

	// Newest match wins, and it must extend what was typed.
	if got := suggest("make "); got != "make test && make lint" {
		t.Errorf("suggest(%q) = %q, want the newest matching entry", "make ", got)
	}
	if got := suggest("nomatch"); got != "" {
		t.Errorf("suggest of an unmatched prefix = %q, want empty", got)
	}

	// The knob is read per call, so an rc line or a live `config`
	// change takes effect without rebuilding the editor.
	off := suggestFn(runnerWithVars(t, map[string]string{"KOI_SUGGEST": "off"}), store)
	if got := off("make "); got != "" {
		t.Errorf("KOI_SUGGEST=off still suggested %q", got)
	}
}

// transientPrompt is gated twice — on the active theme and on the p10k
// engine's own TRANSIENT_PROMPT setting — and both gates were untested.
//
// XDG_CONFIG_HOME is redirected because p10kConfigFor layers the user's
// real p10k.conf over the preset. Without that, this test reads whatever
// the developer happens to have configured: the first version passed
// locally purely because the author's own config sets TRANSIENT_PROMPT,
// and failed on every CI runner, where it defaults to off. A test that
// reaches into real user state is exactly what AGENTS.md forbids, and
// this is what it looks like when it slips through.
func TestTransientPromptFollowsTheTheme(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	info := promptInfo{}

	// The setting is off by default, for every theme.
	if got := transientPrompt(runnerWithVars(t, map[string]string{"KOI_THEME": "p10k"}), info); got != "" {
		t.Errorf("transient prompt is off by default, got %q", got)
	}

	// Turned on in the session, p10k renders one.
	on := map[string]string{"KOI_THEME": "p10k", "POWERLEVEL9K_TRANSIENT_PROMPT": "always"}
	if got := transientPrompt(runnerWithVars(t, on), info); got == "" {
		t.Error("p10k with TRANSIENT_PROMPT=always produced nothing")
	}

	// Every other theme gets nothing even with the setting on: only the
	// p10k engine implements a transient prompt today.
	for _, theme := range []string{"plain", "koi", "starship", ""} {
		vars := map[string]string{"KOI_THEME": theme, "POWERLEVEL9K_TRANSIENT_PROMPT": "always"}
		if got := transientPrompt(runnerWithVars(t, vars), info); got != "" {
			t.Errorf("KOI_THEME=%q produced a transient prompt %q; only p10k implements one", theme, got)
		}
	}

	// A manual prompt outranks every theme, transient included.
	manual := map[string]string{
		"KOI_THEME": "p10k", "KOI_PROMPT": "> ",
		"POWERLEVEL9K_TRANSIENT_PROMPT": "always",
	}
	if got := transientPrompt(runnerWithVars(t, manual), info); got != "" {
		t.Errorf("manual KOI_PROMPT still got a transient prompt %q", got)
	}
}

// editModeOf decides whether the session is emacs or vi. `set -o vi` in
// an inherited rc is the case that matters, and it reaches this through
// KOI_EDIT_MODE.
func TestEditModeOf(t *testing.T) {
	for value, want := range map[string]editor.EditMode{
		"":         editor.ModeEmacs,
		"emacs":    editor.ModeEmacs,
		"vi":       editor.ModeVi,
		"VI":       editor.ModeVi, // case is not a configuration error
		"nonsense": editor.ModeEmacs,
	} {
		runner := runnerWithVars(t, map[string]string{"KOI_EDIT_MODE": value})
		if got := editModeOf(runner); got != want {
			t.Errorf("KOI_EDIT_MODE=%q resolved to %v, want %v", value, got, want)
		}
	}
}

// A runner is not required to have run anything before the editor asks
// for its hooks: the first prompt is resolved before any command. A nil
// Vars map is the shape that produced the KOI_THEME no-op bug (#45).
func TestHooksSurviveAFreshRunner(t *testing.T) {
	runner, err := interp.New()
	if err != nil {
		t.Fatal(err)
	}
	store := storeWith(t, "echo hi")

	if got := suggestFn(runner, store)("ec"); got != "echo hi" {
		t.Errorf("fresh runner suggest = %q, want %q", got, "echo hi")
	}
	if got := editModeOf(runner); got != editor.ModeEmacs {
		t.Errorf("fresh runner edit mode = %v, want emacs", got)
	}
	_ = transientPrompt(runner, promptInfo{}) // must not panic
}
