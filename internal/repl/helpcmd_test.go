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
