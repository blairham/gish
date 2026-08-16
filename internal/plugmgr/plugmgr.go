// Package plugmgr is gish's native plugin manager: the zi-go engine
// (github.com/blairham/zi-go) moved in-tree per issue #23. The zsh shim
// that drove upstream zi-go is gone — gish sources payloads directly.
//
// The Manager interface is the replaceability contract: the in-tree zi
// engine is the compiled-in default (a manager that must be installed by
// a manager is a chicken-and-egg), and an alternative can claim the same
// contract from a tier-2 plugin once CommandProvider (#11) lands. The
// shell's side never changes: it sources whatever payload Load returns.
package plugmgr

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/blairham/gish/internal/plugmgr/config"
	"github.com/blairham/gish/internal/plugmgr/emit"
	"github.com/blairham/gish/internal/plugmgr/ice"
	"github.com/blairham/gish/internal/plugmgr/installer"
	"github.com/blairham/gish/internal/plugmgr/spec"
	"github.com/blairham/gish/internal/plugmgr/state"
)

// Manager is the plugin-manager contract. Load and Snippet install as
// needed and return the path of a payload script for the shell to
// source; management verbs report through the writer they were given.
type Manager interface {
	// SetIces buffers ice modifiers for the next Load/Snippet, zi-style.
	SetIces(args []string) error
	// Load installs (if needed) a plugin and returns its payload path.
	Load(rawSpec string) (string, error)
	// Snippet installs (if needed) a snippet and returns its payload path.
	Snippet(rawSpec string) (string, error)
	// Update refreshes one object (or all when target is empty).
	Update(target string, out io.Writer) error
	// Delete removes an installed object.
	Delete(target string) error
	// List writes the installed objects.
	List(out io.Writer) error
}

// Zi is the default Manager: the ported zi-go engine. Layout and
// $ZI_GO_HOME match standalone zi-go, so existing installs carry over.
type Zi struct {
	cfg     *config.Config
	inst    *installer.Installer
	pending *ice.Ices
}

// NewZi creates the default manager. out receives install progress.
func NewZi(out io.Writer) (*Zi, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return &Zi{
		cfg:     cfg,
		inst:    &installer.Installer{Cfg: cfg, Out: out},
		pending: ice.New(),
	}, nil
}

// Home returns the manager's root directory (for status displays).
func (z *Zi) Home() string { return z.cfg.Home }

func (z *Zi) SetIces(args []string) error {
	ic, err := ice.ParseArgs(args)
	if err != nil {
		return err
	}
	z.pending = ic
	return nil
}

// takeIces consumes the buffered ices — zi semantics: an `zi ice` line
// applies to exactly the next load/snippet.
func (z *Zi) takeIces() *ice.Ices {
	ic := z.pending
	z.pending = ice.New()
	return ic
}

func (z *Zi) Load(rawSpec string) (string, error) {
	ic := z.takeIces()
	s, err := spec.ParsePlugin(rawSpec, ic.Get("from"), ic.Get("id-as"))
	if err != nil {
		return "", err
	}
	dir, _, err := z.inst.EnsurePlugin(s, ic)
	if err != nil {
		return "", err
	}
	var sourceFile string
	switch ic.Get("as") {
	case "program":
		sourceFile, err = emit.ResolveProgram(dir, s.Repo, ic)
	case "null", "completion":
		// Nothing to resolve.
	default:
		sourceFile, err = emit.ResolveSourceFile(dir, s.Repo, ic)
	}
	if err != nil {
		return "", err
	}
	if !ic.Has("nocompletions") && ic.Get("as") != "null" {
		if cerr := z.linkCompletions(dir); cerr != nil {
			fmt.Fprintf(z.inst.Out, "completions for %s: %v\n", s.Raw, cerr)
		}
	}
	return z.writePayload(s.ID, emit.PluginPayload(s, dir, sourceFile, ic))
}

func (z *Zi) Snippet(rawSpec string) (string, error) {
	ic := z.takeIces()
	s, err := spec.ParseSnippet(rawSpec, ic.Get("id-as"))
	if err != nil {
		return "", err
	}
	file, _, err := z.inst.EnsureSnippet(s, ic)
	if err != nil {
		return "", err
	}
	return z.writePayload(s.ID, emit.SnippetPayload(s, file, ic))
}

// writePayload stores the load script under run/. Upstream printed an
// eval line for the shim here; gish just sources the file.
func (z *Zi) writePayload(id, payload string) (string, error) {
	runFile := filepath.Join(z.cfg.RunDir(), id+".gish")
	if err := os.WriteFile(runFile, []byte(payload), 0o644); err != nil {
		return "", err
	}
	return runFile, nil
}

// ziUpdateJobs bounds the update fan-out. Updates are network-bound
// (git pulls, release fetches), not CPU-bound, so the width is a fixed
// politeness cap rather than a core count.
const ziUpdateJobs = 8

// UpdateEvent reports one object's lifecycle in the update fan-out —
// the seam the live board renders from (#90). Queued events arrive for
// every object before work starts, in listing order.
type UpdateEvent struct {
	Index   int
	Name    string
	State   UpdateState
	Outcome string // Done only
	Failed  bool   // Done only
}

// UpdateState is an UpdateEvent's lifecycle position.
type UpdateState int

// The lifecycle: queued (announced upfront), started (a worker picked
// it), done (outcome known).
const (
	UpdateQueued UpdateState = iota
	UpdateStarted
	UpdateDone
)

// ProgressUpdater is the optional Manager extension a live progress
// display uses; managers without it get the plain line output.
type ProgressUpdater interface {
	UpdateWithProgress(target string, out io.Writer, observe func(UpdateEvent)) error
}

// Update refreshes one object, or every installed object concurrently
// (#49): the same FIFO-admission, ordered-flush pool discipline as the
// parallel builtin. Status lines print in listing order as each
// object's turn completes; per-object hook output (make/atpull)
// streams to the installer's writer as it happens.
func (z *Zi) Update(target string, out io.Writer) error {
	return z.UpdateWithProgress(target, out, nil)
}

// UpdateWithProgress is Update with an observer: when observe is
// non-nil it becomes the display (line output is suppressed) and is
// called from worker goroutines — observers must be safe for that.
func (z *Zi) UpdateWithProgress(target string, out io.Writer, observe func(UpdateEvent)) error {
	var dirs []string
	if target == "" {
		for _, root := range []string{z.cfg.PluginsDir(), z.cfg.SnippetsDir()} {
			entries, err := os.ReadDir(root)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() {
					dirs = append(dirs, filepath.Join(root, e.Name()))
				}
			}
		}
	} else {
		dir, _, err := z.findObject(target)
		if err != nil {
			return err
		}
		dirs = []string{dir}
	}

	if observe != nil {
		for i, dir := range dirs {
			observe(UpdateEvent{Index: i, Name: filepath.Base(dir), State: UpdateQueued})
		}
	}
	results := make([]string, len(dirs))
	done := make([]chan struct{}, len(dirs))
	for i := range done {
		done[i] = make(chan struct{})
	}
	tasks := make(chan int)
	var workers sync.WaitGroup
	for range min(ziUpdateJobs, len(dirs)) {
		workers.Go(func() {
			for i := range tasks {
				if observe != nil {
					observe(UpdateEvent{Index: i, Name: filepath.Base(dirs[i]), State: UpdateStarted})
				}
				outcome, err := z.inst.Update(dirs[i])
				if err != nil {
					outcome = err.Error()
				}
				if observe != nil {
					observe(UpdateEvent{
						Index: i, Name: filepath.Base(dirs[i]),
						State: UpdateDone, Outcome: outcome, Failed: err != nil,
					})
				}
				results[i] = fmt.Sprintf("%-40s %s\n", filepath.Base(dirs[i]), outcome)
				close(done[i])
			}
		})
	}
	// Workers never write to out, so feeding everything before flushing
	// cannot deadlock; the flush then streams each line at its turn.
	for i := range dirs {
		tasks <- i
	}
	close(tasks)
	for i := range dirs {
		<-done[i]
		if observe == nil { // the observer is the display
			io.WriteString(out, results[i]) //nolint:errcheck // status display only
		}
	}
	workers.Wait()
	return nil
}

func (z *Zi) Delete(target string) error {
	dir, _, err := z.findObject(target)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// ObjectInfo is one installed object for structured listings (#90).
type ObjectInfo struct {
	Kind, Raw, Ices string
}

// ObjectLister is the optional Manager extension structured views use;
// managers without it get the plain List output.
type ObjectLister interface {
	Objects() ([]ObjectInfo, error)
}

// Objects returns the installed objects, plugins then snippets.
func (z *Zi) Objects() ([]ObjectInfo, error) {
	var out []ObjectInfo
	for _, root := range []struct{ kind, dir string }{
		{"plugin", z.cfg.PluginsDir()}, {"snippet", z.cfg.SnippetsDir()},
	} {
		objs, err := state.ListObjects(root.dir, root.kind)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			out = append(out, ObjectInfo{Kind: o.Kind, Raw: o.Raw, Ices: ice.FromMap(o.Ices).String()})
		}
	}
	return out, nil
}

func (z *Zi) List(out io.Writer) error {
	for kind, root := range map[string]string{"plugin": z.cfg.PluginsDir(), "snippet": z.cfg.SnippetsDir()} {
		objs, err := state.ListObjects(root, kind)
		if err != nil {
			return err
		}
		for _, o := range objs {
			ices := ice.FromMap(o.Ices).String()
			if ices != "" {
				ices = "  [" + ices + "]"
			}
			fmt.Fprintf(out, "%-8s %s%s\n", o.Kind, o.Raw, ices)
		}
	}
	return nil
}

// findObject locates an installed object by raw spec, ID, or unique
// substring (ported from upstream's helpers).
func (z *Zi) findObject(query string) (string, *state.Object, error) {
	roots := map[string]string{"plugin": z.cfg.PluginsDir(), "snippet": z.cfg.SnippetsDir()}
	var matches []string
	var found *state.Object
	for kind, root := range roots {
		objs, err := state.ListObjects(root, kind)
		if err != nil {
			return "", nil, err
		}
		for _, o := range objs {
			if o.Raw == query || o.ID == query || o.ID == spec.Sanitize(query) {
				return filepath.Join(root, o.ID), o, nil
			}
			if strings.Contains(o.ID, spec.Sanitize(query)) || strings.Contains(o.Raw, query) {
				matches = append(matches, filepath.Join(root, o.ID))
				found = o
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", nil, fmt.Errorf("no installed object matches %q", query)
	case 1:
		return matches[0], found, nil
	default:
		return "", nil, fmt.Errorf("%q is ambiguous: %s", query, strings.Join(matches, ", "))
	}
}

// linkCompletions symlinks _* completion files into the shared
// completions dir (consumed by the completion engine at M4).
func (z *Zi) linkCompletions(dir string) error {
	var files []string
	for _, pattern := range []string{"_*", "completions/_*", "src/_*"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		files = append(files, matches...)
	}
	for _, f := range files {
		if st, err := os.Stat(f); err != nil || st.IsDir() {
			continue
		}
		link := filepath.Join(z.cfg.CompletionsDir(), filepath.Base(f))
		os.Remove(link)
		if err := os.Symlink(f, link); err != nil {
			return err
		}
	}
	return nil
}
