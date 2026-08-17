package promptengine

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Context is everything a segment is allowed to know. It is passed by
// pointer and must be treated as read-only.
//
// The hard rule of this package lives here: a segment may read this
// struct, environment variables, and files. It may not fork, dial, or
// block. Anything that needs a subprocess or a network round trip is
// somebody else's job — it arrives here pre-computed and possibly stale
// (see [Context.Git]), because a prompt that waits is a prompt that is
// slower than the thing we are replacing.
type Context struct {
	Cwd      string
	Home     string
	Username string
	Hostname string

	// ExitCode and Duration describe the last foreground pipeline.
	ExitCode int
	Duration time.Duration

	// Jobs is the number of running or stopped jobs.
	Jobs int

	// Width is the terminal width in columns; 0 means unknown, and the
	// layout then skips anything that needs to know where the edge is.
	Width int

	Root bool
	SSH  bool

	// Now is the render clock, injectable so tests are not flaky.
	Now time.Time

	// Git is the version-control status, supplied by whoever owns the
	// repository scan. Nil means "not a repo, or not known yet"; a
	// segment must render nothing rather than wait for it.
	Git *GitStatus

	// Getenv is the environment lookup. It is a function rather than a
	// map because the plugin front door serves an allowlisted subset,
	// and segments must not be able to tell the difference.
	Getenv func(string) string

	// PrevCwd is the directory the previous prompt rendered in, which is
	// how the same-dir transient mode tells "I moved" from "I stayed".
	PrevCwd string

	// upCache memoizes FindUp for the life of this render.
	upCache map[string]string
	// statCache memoizes exists() the same way, for the settings that
	// have to touch the filesystem (SHORTEN_FOLDER_MARKER, #133).
	statCache map[string]bool
}

// Env reads an environment variable through the context's lookup.
func (c *Context) Env(name string) string {
	if c.Getenv == nil {
		return os.Getenv(name)
	}
	return c.Getenv(name)
}

// FindUp looks for a file in the current directory and its ancestors,
// returning its path and whether it was found.
//
// A prompt can easily carry a dozen version-manager segments, and each
// one wants the nearest .python-version or .tool-versions or stack.yaml.
// Walking independently would be a hundred stats per keystroke's worth
// of prompt, so the answers are memoized for the life of one Context —
// which is exactly one render, so nothing is ever served stale.
func (c *Context) FindUp(name string) (string, bool) {
	if c.upCache == nil {
		c.upCache = map[string]string{}
	}
	if hit, ok := c.upCache[name]; ok {
		return hit, hit != ""
	}
	found := ""
	dir := c.Cwd
	for range maxWalkUp {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			found = candidate
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	c.upCache[name] = found
	return found, found != ""
}

// exists reports whether a path is there, memoized for this render.
//
// Same reasoning as FindUp: SHORTEN_FOLDER_MARKER asks the question once
// per component per marker, and a deep path with two markers is a dozen
// stats that a repeated prompt in the same directory should not repeat.
func (c *Context) exists(path string) bool {
	if c.statCache == nil {
		c.statCache = map[string]bool{}
	}
	if hit, ok := c.statCache[path]; ok {
		return hit
	}
	_, err := os.Stat(path)
	found := err == nil
	c.statCache[path] = found
	return found
}

// maxWalkUp bounds the ancestor search. Deep enough for any real project
// layout, shallow enough that a prompt in a pathological directory does
// not turn into a filesystem crawl.
const maxWalkUp = 24

// firstLine reads the first line of a file, which is the shape every
// version manager stores its pin in.
func firstLine(path string) string {
	line, _, _ := strings.Cut(readFile(path), "\n")
	return strings.TrimSpace(line)
}

// readFile reads a small file, treating any failure as absence. Every
// caller here is asking "is this pinned?", and an unreadable pin file
// answers that question the same way a missing one does.
func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// clock returns the render time, defaulting to now.
func (c *Context) clock() time.Time {
	if c.Now.IsZero() {
		return time.Now()
	}
	return c.Now
}

// GitStatus is the repository state a prompt needs. It mirrors what the
// gish-git plugin already computes, so the two agree on vocabulary.
type GitStatus struct {
	// Dir is the repository root, used to tell "this status is for the
	// directory I am rendering" from a stale answer for somewhere else.
	Dir string

	Branch     string // local branch, empty when detached
	Tag        string
	Commit     string // short hash, shown when detached and untagged
	RemoteRef  string // upstream branch when it differs from the local one
	Action     string // in-progress operation: merge, rebase, cherry-pick
	Ahead      int
	Behind     int
	PushAhead  int
	PushBehind int
	Stashed    int
	Conflicted int
	Staged     int
	Modified   int
	Untracked  int

	// Stale marks a status served from cache while a refresh runs. The
	// vcs segment dims itself rather than lying about freshness.
	Stale bool
}

// Clean reports whether the working tree has nothing outstanding.
func (g *GitStatus) Clean() bool {
	return g.Conflicted == 0 && g.Staged == 0 && g.Modified == 0 && g.Untracked == 0
}

// Span is a run of text with its own foreground. Segments that need
// more than one color — the vcs counters, the shortened parts of a
// path — return spans rather than pre-colored strings.
//
// This is not a stylistic preference. A segment that bakes its own
// escapes has to end them with a reset, and a reset also clears the
// *background* the layout put underneath it, which is exactly how a
// powerline preset ends up with holes punched in its ribbon. Keeping
// color as data until the layout runs is what makes the two composable.
type Span struct {
	Text string
	Fg   Color
	Bold bool
}

// Rendered is one segment's answer for one prompt.
type Rendered struct {
	// Content is the text for the common case of a single color.
	Content string

	// Spans, when non-empty, replaces Content: the segment is describing
	// its own colors run by run, and the layout supplies the background.
	Spans []Span

	// Icon is the visual identifier shown before or after the content.
	Icon string

	// State selects the middle step of the config fallback chain, so a
	// segment can be colored differently per condition: the dir segment
	// reports NOT_WRITABLE, the vcs segment CLEAN or MODIFIED.
	State string
}

// RenderFunc computes a segment. ok=false means "I have nothing to say
// here" — the segment vanishes and takes its separators with it, which
// is how a prompt stays quiet in directories where a tool is irrelevant.
type RenderFunc func(cfg *Config, ctx *Context) (Rendered, bool)

// registry maps element names to their implementations. Names match the
// upstream spelling exactly, because that is what users type into their
// element lists and what every guide on the internet refers to.
var registry = map[string]RenderFunc{}

// register adds a segment. It panics on a duplicate name: two segments
// answering to one name is a build-time mistake, not a runtime state.
func register(name string, fn RenderFunc) {
	if _, dup := registry[name]; dup {
		panic("p10k: duplicate segment " + name)
	}
	registry[name] = fn
}

// Segments lists every implemented segment name, sorted.
func Segments() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Known reports whether a name is an implemented segment or one of the
// layout pseudo-elements.
func Known(name string) bool {
	if name == elementNewline {
		return true
	}
	_, ok := registry[strings.TrimSpace(name)]
	return ok
}

// elementNewline is the pseudo-element that ends a prompt line. It is
// not a segment: it renders nothing and carries no colors.
const elementNewline = "newline"
