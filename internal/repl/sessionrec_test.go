package repl

import (
	"testing"

	"github.com/blairham/koi-shell/internal/session"
)

// The recorder's whole job is to notice what changed. These drive it
// directly, because the interactive loop is not the place to find out
// that a diff was empty.
func TestRecorderCapturesEnvDiffAndCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	runner := newTestRunner(t)
	rec := newSessionRecorder("sess-test", runner, func() []string { return nil })
	if rec == nil {
		t.Fatal("recorder unavailable")
	}

	// A variable exported after the baseline is the session's own diff.
	if err := runEnvScript(t.Context(), runner, "export MY_PROJECT=demo\n"); err != nil {
		t.Fatal(err)
	}
	rec.atPrompt(runner, "export MY_PROJECT=demo")

	got := rec.store.List()
	if len(got) != 1 {
		t.Fatalf("recorded %d sessions", len(got))
	}
	if got[0].Env["MY_PROJECT"] != "demo" {
		t.Errorf("env diff missing the exported variable: %+v", got[0].Env)
	}
	if got[0].LastCommand != "export MY_PROJECT=demo" {
		t.Errorf("last command = %q", got[0].LastCommand)
	}
}

// Inherited variables are not the session's diff: recording them would
// persist the login shell's whole environment on every prompt.
func TestRecorderIgnoresBaselineEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	runner := newTestRunner(t)
	if err := runEnvScript(t.Context(), runner, "export INHERITED=yes\n"); err != nil {
		t.Fatal(err)
	}
	rec := newSessionRecorder("sess-test", runner, nil) // baseline taken now
	rec.atPrompt(runner, "true")

	got := rec.store.List()
	if len(got) != 1 {
		t.Fatalf("recorded %d sessions", len(got))
	}
	if _, ok := got[0].Env["INHERITED"]; ok {
		t.Errorf("baseline variable was recorded as a diff: %+v", got[0].Env)
	}
}

// A session idle at a prompt does no I/O.
func TestRecorderSkipsUnchangedState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	runner := newTestRunner(t)
	rec := newSessionRecorder("sess-test", runner, nil)
	rec.atPrompt(runner, "ls")
	first := rec.store.List()[0].UpdatedUnixMs

	rec.atPrompt(runner, "ls") // nothing changed
	if again := rec.store.List()[0].UpdatedUnixMs; again != first {
		t.Error("an unchanged prompt rewrote the record")
	}

	rec.atPrompt(runner, "cd /") // something changed
	if again := rec.store.List()[0].LastCommand; again != "cd /" {
		t.Errorf("a changed prompt did not rewrite: %q", again)
	}
}

func TestChangedIgnoresTimestamp(t *testing.T) {
	a := session.Record{ID: "x", Cwd: "/a", UpdatedUnixMs: 1}
	b := session.Record{ID: "x", Cwd: "/a", UpdatedUnixMs: 999}
	if changed(a, b) {
		t.Error("a new timestamp alone counted as a change; that would write a file every prompt")
	}
	if !changed(a, session.Record{ID: "x", Cwd: "/b"}) {
		t.Error("a new cwd was not seen as a change")
	}
}
