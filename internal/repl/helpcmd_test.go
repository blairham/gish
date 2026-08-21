package repl

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/builtins"
)

// runHelpScript runs src through the full RunReader path, which stacks
// overrideCallHandler and every command handler runHelp rewrites into.
func runHelpScript(t *testing.T, src string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("KOI_RC", filepath.Join(t.TempDir(), "koirc"))
	var out, errOut strings.Builder
	err = RunReader(t.Context(), strings.NewReader(src), "test",
		interp.StdIO(nil, &out, &errOut))
	return out.String(), errOut.String(), err
}

// TestHelpCoversEveryBuiltin is the drift guard: a builtin that ships
// without a help topic is undiscoverable by the command that exists to
// discover it.
func TestHelpCoversEveryBuiltin(t *testing.T) {
	t.Parallel()
	for _, name := range builtins.ShellBuiltins() {
		if _, ok := helpTopics[name]; !ok {
			t.Errorf("shell builtin %q has no help topic", name)
		}
	}
	// The native names registered per-session (jobs/fg/bg, kill, ...) are
	// not visible from a bare test process, so the full set is spelled
	// out; nativebuiltins_test.go in cmd/koi keeps this list honest.
	natives := []string{
		"bg", "builtins", "fc", "fg", "help", "jobs", "kill", "newgrp",
		"parallel", "plugins", "times", "umask",
	}
	for _, name := range natives {
		if _, ok := helpTopics[name]; !ok {
			t.Errorf("native builtin %q has no help topic", name)
		}
	}
	for _, name := range callHandlerCommands {
		if _, ok := helpTopics[name]; !ok && !slices.Contains(helpRewrites, name) {
			t.Errorf("koi command %q has neither a help topic nor a rewrite", name)
		}
	}
}

// TestHelpTopicsNameRealCommands is the reverse check: a topic or rewrite
// for a name nothing answers is a lie.
func TestHelpTopicsNameRealCommands(t *testing.T) {
	t.Parallel()
	known := builtins.ShellBuiltins()
	known = append(known,
		"bg", "builtins", "fc", "fg", "help", "jobs", "kill", "newgrp",
		"parallel", "plugins", "times", "umask")
	known = append(known, callHandlerCommands...)
	for name := range helpTopics {
		if !slices.Contains(known, name) {
			t.Errorf("help topic %q names nothing the session answers", name)
		}
	}
	for _, name := range helpRewrites {
		if !slices.Contains(callHandlerCommands, name) {
			t.Errorf("help rewrite %q is not a CallHandler command", name)
		}
	}
}

func TestHelpExplainsAShellBuiltin(t *testing.T) {
	out, _, err := runHelpScript(t, "help cd\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "change the working directory") {
		t.Errorf("help cd = %q", out)
	}
}

func TestHelpRewritesToACommandHelpScreen(t *testing.T) {
	out, _, err := runHelpScript(t, "help config\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "usage: config") {
		t.Errorf("help config did not reach config's own usage: %q", out)
	}
}

func TestHelpOverviewListsTheGroups(t *testing.T) {
	out, _, err := runHelpScript(t, "help\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"koi commands:", "koi builtins:", "shell builtins:"} {
		if !strings.Contains(out, group) {
			t.Errorf("overview missing %q: %q", group, out)
		}
	}
}

func TestHelpUnknownNameFails(t *testing.T) {
	_, errOut, err := runHelpScript(t, "help nosuchthing\n")
	if err == nil {
		t.Fatal("help nosuchthing succeeded")
	}
	var status interp.ExitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Fatalf("err = %v, want exit status 1", err)
	}
	if !strings.Contains(errOut, "no help topic") || !strings.Contains(errOut, "man nosuchthing") {
		t.Errorf("stderr = %q", errOut)
	}
}

// help must keep the shell alive on failure — the exact hazard the
// overrides.go comment records for CallHandler errors.
func TestHelpFailureIsNotFatal(t *testing.T) {
	out, _, err := runHelpScript(t, "help nosuchthing || echo survived\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "survived") {
		t.Errorf("the || branch never ran: %q", out)
	}
}

// TestHelpSyntaxTableIsWellFormed guards the shape of the syntax half
// (#557). It is a separate table from helpTopics because those names are
// commands and these are grammar, so the two must not overlap, and every
// entry has to carry both halves of the answer — a topic that is listed
// with no text is the bug this issue was about, arriving from the other
// side. Whether koi actually *runs* each construct is asserted where a
// script can run, in cmd/koi/nativebuiltins_test.go.
func TestHelpSyntaxTableIsWellFormed(t *testing.T) {
	t.Parallel()
	for name, topic := range helpSyntaxTopics {
		if _, clash := helpTopics[name]; clash {
			t.Errorf("%q is both a command topic and a syntax topic", name)
		}
		if strings.TrimSpace(topic.use) == "" {
			t.Errorf("syntax topic %q has no synopsis", name)
		}
		if len(strings.TrimSpace(topic.desc)) < 20 {
			t.Errorf("syntax topic %q is described as %q", name, topic.desc)
		}
	}
	for alias, canonical := range helpSyntaxAliases {
		if _, ok := helpSyntaxTopics[canonical]; !ok {
			t.Errorf("alias %q points at %q, which is not a topic", alias, canonical)
		}
		if _, listed := helpSyntaxTopics[alias]; listed {
			t.Errorf("alias %q is also a listed topic", alias)
		}
		if !slices.Contains(helpTopicNames(), canonical) {
			t.Errorf("%q resolves but is not in the listing", canonical)
		}
		if slices.Contains(helpTopicNames(), alias) {
			t.Errorf("alias %q is offered by the listing; only the canonical name should be", alias)
		}
	}
}

func TestHelpExplainsShellSyntax(t *testing.T) {
	out, _, err := runHelpScript(t, "help while\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "while commands ; do") {
		t.Errorf("help while = %q", out)
	}
}

// A punctuation topic is reached by the form a person types, and the
// entry is headed by the name the listing offers — bash's own behavior,
// which it gets from prefix-matching its table.
func TestHelpPunctuationAliasNamesTheListedTopic(t *testing.T) {
	out, _, err := runHelpScript(t, "help '[['\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "[[ ... ]]: ") {
		t.Errorf("help '[[' = %q, want the entry headed by the listed name", out)
	}
}

func TestHelpOverviewListsTheSyntaxGroup(t *testing.T) {
	out, _, err := runHelpScript(t, "help\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "shell syntax:") {
		t.Errorf("overview has no syntax group: %q", out)
	}
	for _, name := range []string{"case", "while", "for ((", "[[ ... ]]"} {
		if !strings.Contains(out, name) {
			t.Errorf("overview does not name the %q topic: %q", name, out)
		}
	}
}
