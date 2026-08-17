package repl

import (
	"context"
	"slices"
	"testing"
)

// The alias mirror records only what proves a name exists: the defining
// form. Queries, flags, and other commands entirely must leave it alone.
func TestAliasNamesObserve(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		calls [][]string
		want  []string
	}{
		{"define", [][]string{{"alias", "ll=ls -la"}}, []string{"ll"}},
		{"define several", [][]string{{"alias", "a=1", "b=2"}}, []string{"a", "b"}},
		{"query is not a definition", [][]string{{"alias", "ll"}}, nil},
		{"flags are not names", [][]string{{"alias", "-p"}}, nil},
		{"empty name ignored", [][]string{{"alias", "=oops"}}, nil},
		{"other commands ignored", [][]string{{"export", "x=y"}, {"echo", "a=b"}}, nil},
		{"unalias removes", [][]string{{"alias", "a=1", "b=2"}, {"unalias", "a"}}, []string{"b"}},
		{"unalias -a clears", [][]string{{"alias", "a=1", "b=2"}, {"unalias", "-a"}}, nil},
		{"redefine keeps one", [][]string{{"alias", "a=1"}, {"alias", "a=2"}}, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var a aliasNames
			for _, call := range tt.calls {
				a.observe(call)
			}
			if got := a.names(); !slices.Equal(got, tt.want) {
				t.Errorf("names() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The tracker is an observer: whatever it learns, the words continue to
// the chain below unchanged.
func TestAliasTrackCallHandlerObservesAndPassesThrough(t *testing.T) {
	t.Cleanup(sessionAliases.reset)
	sessionAliases.reset()

	next := func(_ context.Context, args []string) ([]string, error) { return args, nil }
	in := []string{"alias", "gg=git status"}
	out, err := aliasTrackCallHandler(next)(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out, in) {
		t.Errorf("args rewritten: got %v, want %v", out, in)
	}
	if !slices.Contains(sessionAliases.names(), "gg") {
		t.Errorf("alias %q not observed; mirror has %v", "gg", sessionAliases.names())
	}
}

// One list answers "what can this session run" for every name-judging
// surface (#193). The cases here are one representative per source the
// old private lists each missed some of.
func TestSessionCommandNamesCoversEverySource(t *testing.T) {
	t.Cleanup(sessionAliases.reset)
	sessionAliases.reset()
	sessionAliases.observe([]string{"alias", "ll=ls -la"})

	runner := runnerWithVars(t, nil)
	runIn(t, runner, "myfn() { :; }")

	names := sessionCommandNames(runner)
	for _, want := range []string{
		"cd",       // implemented interpreter builtin
		"builtins", // gish-native builtin on the exec seam
		"parallel", // gish-native builtin on the exec seam
		"doctor",   // CallHandler-routed command
		"trust",    // CallHandler-routed command
		"myfn",     // session function
		"ll",       // alias
	} {
		if !slices.Contains(names, want) {
			t.Errorf("sessionCommandNames missing %q", want)
		}
	}
}

// knownCommand is the highlighter's verdict; every name above must not
// paint red, and a genuinely absent name still must.
func TestKnownCommandVerdicts(t *testing.T) {
	t.Cleanup(sessionAliases.reset)
	sessionAliases.reset()
	sessionAliases.observe([]string{"alias", "ll=ls -la"})

	runner := runnerWithVars(t, nil)
	for _, name := range []string{"cd", "newgrp", "builtins", "doctor", "zi", "config", "ll"} {
		if !knownCommand(runner, name) {
			t.Errorf("knownCommand(%q) = false, want true", name)
		}
	}
	if knownCommand(runner, "definitely-not-a-command-zz") {
		t.Error(`knownCommand("definitely-not-a-command-zz") = true, want false`)
	}
}
