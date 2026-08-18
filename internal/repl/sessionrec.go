package repl

import (
	"os"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/koi-shell/internal/session"
)

// Recording the live session (#103).
//
// The write happens at the prompt — after a command finishes, before the
// next one is typed — which is both the moment the state is meaningful
// and the moment the shell is otherwise idle. It is a small file written
// with the same write-then-rename discipline as every other piece of
// koi state, so a shell killed mid-write leaves the previous record
// intact rather than a truncated one.
//
// Recording is skipped entirely when nothing has changed, so a session
// sitting at a prompt does no I/O at all.

// sessionRecorder persists one session's state.
type sessionRecorder struct {
	store *session.Store
	id    string
	// baseline is the environment at startup, so what gets recorded is
	// the session's own *diff* rather than an inherited copy of every
	// variable the login shell exported.
	baseline map[string]string
	last     session.Record
	// jobsOf reports background job command lines. Injected rather than
	// reached for globally: the job table is owned by the interactive
	// loop, and tests supply their own.
	jobsOf func() []string
	// off disables recording after a write failure, so a broken state
	// directory produces one silent skip rather than an error per
	// prompt for the rest of the session.
	off bool
}

// sessionMgr is the live recorder, nil when recording is unavailable.
var sessionMgr *sessionRecorder

// newSessionRecorder captures the baseline environment. A store that
// cannot be opened disables the feature silently: session restore is a
// convenience, and no part of starting a shell should depend on it.
func newSessionRecorder(id string, runner *interp.Runner, jobsOf func() []string) *sessionRecorder {
	store, err := session.OpenDefault()
	if err != nil {
		return nil
	}
	return &sessionRecorder{store: store, id: id, baseline: snapshotEnv(runner), jobsOf: jobsOf}
}

// snapshotEnv reads the runner's exported variables.
//
// Both sources are consulted on purpose. runner.Vars holds what the
// session assigned; runner.Env holds what it inherited and exported.
// Reading only Vars is the same mistake that made KOI_THEME=p10k a
// no-op until #45 — a variable can be perfectly set and simply not be
// in the map you happened to look at.
func snapshotEnv(runner *interp.Runner) map[string]string {
	out := map[string]string{}
	if runner == nil {
		return out
	}
	if runner.Env != nil {
		runner.Env.Each(func(name string, v expand.Variable) bool {
			if v.IsSet() && v.Exported {
				out[name] = v.String()
			}
			return true
		})
	}
	for name, v := range runner.Vars {
		if v.IsSet() && v.Exported {
			out[name] = v.String()
		}
	}
	return out
}

// atPrompt records the session if anything worth recording changed.
func (s *sessionRecorder) atPrompt(runner *interp.Runner, lastCommand string) {
	if s == nil || s.off {
		return
	}
	// Nothing is written before the first command (#163). A session with
	// no command in it has no place to restore *to* that the next shell
	// would not reach anyway, so recording it buys nothing and costs a
	// file — and a state directory — in the home of someone who did no
	// more than open a shell.
	if lastCommand == "" && s.last.ID == "" {
		return
	}
	rec := session.Record{
		ID:            s.id,
		Cwd:           runner.Dir,
		Env:           s.envDiff(runner),
		Pins:          s.pins(),
		Jobs:          s.jobs(),
		LastCommand:   lastCommand,
		UpdatedUnixMs: time.Now().UnixMilli(),
	}
	if !changed(s.last, rec) {
		return // idle at a prompt: no I/O at all
	}
	if err := s.store.Save(rec); err != nil {
		s.off = true
		return
	}
	s.last = rec
}

// envDiff is what this session changed against its own startup
// baseline. Recording the whole environment would mostly persist the
// login shell's exports, which the next shell gets anyway.
func (s *sessionRecorder) envDiff(runner *interp.Runner) map[string]string {
	out := map[string]string{}
	for name, value := range snapshotEnv(runner) {
		if base, ok := s.baseline[name]; ok && base == value {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	// The store filters secrets and loader hooks on write; this is the
	// diff, not the sanitized form.
	return out
}

// pins reports the active tool-version selections (#77).
func (s *sessionRecorder) pins() map[string]string {
	if toolsMgr == nil {
		return nil
	}
	return toolsMgr.activePins()
}

// jobs reports background job command lines as re-runnable text.
// Processes do not survive; their intent does.
func (s *sessionRecorder) jobs() []string {
	if s.jobsOf == nil {
		return nil
	}
	return s.jobsOf()
}

// changed reports whether a record differs from the last one written in
// any way a user would notice. The timestamp is deliberately excluded —
// it changes every prompt, and letting it drive the comparison would
// mean writing a file after every command forever.
func changed(a, b session.Record) bool {
	if a.Cwd != b.Cwd || a.LastCommand != b.LastCommand {
		return true
	}
	if len(a.Env) != len(b.Env) || len(a.Pins) != len(b.Pins) || len(a.Jobs) != len(b.Jobs) {
		return true
	}
	for k, v := range b.Env {
		if a.Env[k] != v {
			return true
		}
	}
	for k, v := range b.Pins {
		if a.Pins[k] != v {
			return true
		}
	}
	for i, j := range b.Jobs {
		if a.Jobs[i] != j {
			return true
		}
	}
	return false
}

// close prunes stale records on the way out. A little I/O on exit costs
// nobody anything, and it is the only moment the shell knows it is done.
func (s *sessionRecorder) close() {
	if s == nil || s.off {
		return
	}
	s.store.Prune(time.Now())
}

// restoreOnStart is the `koi --restore ID` path: land in the recorded
// directory before the first prompt, and let the environment be
// proposed the way `sessions restore` proposes it.
func restoreOnStart(id string, runner *interp.Runner) (detail string, ok bool) {
	store, err := session.OpenDefault()
	if err != nil {
		return "", false
	}
	rec, err := store.Get(id)
	if err != nil {
		return err.Error(), false
	}
	if rec.Cwd == "" {
		return "that record has no directory", false
	}
	if _, err := os.Stat(rec.Cwd); err != nil {
		return "its directory is gone: " + displayPath(rec.Cwd), false
	}
	runner.Dir = rec.Cwd
	if envMgr != nil && len(rec.Env) > 0 {
		if n := envMgr.pendRestored(rec.Cwd, rec.Env); n > 0 {
			return displayPath(rec.Cwd) + " — environment proposed, `trust allow` to apply", true
		}
	}
	return displayPath(rec.Cwd), true
}
