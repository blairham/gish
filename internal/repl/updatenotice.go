package repl

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/blairham/koi-shell/internal/shell/interp"

	"github.com/blairham/koi-shell/internal/ui"
)

// The update notice (#210).
//
// Only two retention loops exist anywhere in this ecosystem's history:
// Oh My Zsh's auto-updater — shipped about three weeks in, and named by
// Robby Russell's own retrospective as the turning point — and atuin's
// sync accounts. A user you can re-touch does not silently churn onto a
// stale, buggier copy of your shell; a user you cannot is acquired once
// and leaks forever. koi has had no re-touch mechanism at all: a brew
// install simply ages.
//
// # It never phones home, and that is not a setting
//
// The obvious build is a version check over the network, opt-in, with
// the URL and cadence documented. That is the wrong shape here. The
// trust stance — open source, local-first, no account, no telemetry — is
// load-bearing (docs/adoption.md: trust is the only failure in the set
// that nobody recovered from), and a shell that dials out on startup
// spends exactly the differentiator the research says converts.
//
// So this reads what the package manager **already fetched**, on the #44
// shellenv posture: stat and string work, no subprocess, no network,
// nothing to opt into. Homebrew keeps a tap as a git checkout whose
// formula carries `version "X.Y.Z"` in plain text, and keeps core
// formula metadata as JSON in its own cache. Both are refreshed by the
// user's own `brew update`. The freshness is theirs; the reading is
// free.
//
// The privacy question dissolves rather than being managed: there is no
// request to authorize, so `config update.notify off` exists to silence
// a line of text, not to withhold consent. It is named `notify` rather
// than the originally proposed `check` for that reason — nothing is
// checked remotely, and a setting that implies otherwise would be its
// own small lie.
//
// # What it can and cannot say
//
// It can say *there is a newer version*. It can never say *you are up to
// date*: local metadata is only as fresh as the last `brew update`, so
// silence means "nothing newer that this machine has heard about". That
// distinction is load-bearing and doctor states it outright, because a
// confident "you're current" from stale data is worse than no answer.

// updateNotifier resolves the locally-known latest version off the
// startup path and reports it once, at a prompt.
type updateNotifier struct {
	// latest is filled by the background resolve; empty until then, and
	// empty forever if nothing local knows.
	latest atomic.Value // string
	shown  atomic.Bool
	source atomic.Value // string: which local file answered
}

// newUpdateNotifier starts the lookup in the background. It never blocks
// the first prompt (#37's budget): a notice that costs startup latency
// would be paying for retention with the thing that wins users.
func newUpdateNotifier() *updateNotifier {
	n := &updateNotifier{}
	go func() {
		latest, source := localLatestVersion()
		if latest != "" {
			n.latest.Store(latest)
			n.source.Store(source)
		}
	}()
	return n
}

// atPrompt prints the notice at most once per session, and only when the
// background lookup has already finished — it never waits for it.
func (n *updateNotifier) atPrompt(runner *interp.Runner, w io.Writer) {
	if n == nil || n.shown.Load() {
		return
	}
	if shellVar(runner, "KOI_UPDATE_NOTIFY", "on") == "off" {
		return
	}
	latest, _ := n.latest.Load().(string)
	if latest == "" || !newerVersion(Version, latest) {
		return
	}
	n.shown.Store(true)
	style := ui.Styles(ui.Enabled(w))
	fmt.Fprintln(w, style.Dim.Render(fmt.Sprintf(
		"koi %s is available (you have %s) — %s", latest, Version, upgradeHint())))
	fmt.Fprintln(w, style.Dim.Render(
		"    "+releaseNotesURL(latest)+"   ·   `config update.notify off` silences this"))
}

// upgradeHint names the command for the way this copy was installed,
// because "upgrade it" is only useful when it is a line you can run.
// Upgrading itself stays delegated to the package manager (#112): a
// self-replacing binary carries obligations koi decided not to own.
func upgradeHint() string {
	if self, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
			self = resolved
		}
		for _, prefix := range brewPrefixes {
			if strings.HasPrefix(self, filepath.Join(prefix, "Cellar")+string(filepath.Separator)) {
				return "`brew upgrade koi`"
			}
		}
	}
	return "upgrade with whatever installed it"
}

func releaseNotesURL(version string) string {
	return "https://github.com/blairham/koi-shell/releases/tag/v" + strings.TrimPrefix(version, "v")
}

// tapVersionRe reads `version "0.1.5"` out of a formula. Homebrew keeps a
// tap as a git checkout, so this is a plain local file that `brew update`
// refreshes.
var tapVersionRe = regexp.MustCompile(`(?m)^\s*version\s+"([^"]+)"`)

// coreVersionRe reads "stable" out of the cached core formula JSON
// without parsing the whole document: the file is one line of several
// kilobytes and only this field is wanted.
var coreVersionRe = regexp.MustCompile(`"versions":\s*\{[^}]*"stable":\s*"([^"]+)"`)

// localLatestVersion asks only files that are already on this machine.
// Returns the version and the file that answered, or empty if nothing
// local knows — which is the normal case for a build from source.
func localLatestVersion() (version, source string) {
	for _, prefix := range brewRoots() {
		// A tap is a git checkout; its formula is plain text.
		matches, _ := filepath.Glob(filepath.Join(prefix, "Library", "Taps", "*", "homebrew-*", "Formula", "koi.rb"))
		for _, path := range matches {
			if v := matchFile(path, tapVersionRe); v != "" {
				return v, path
			}
		}
	}
	for _, path := range brewAPICachePaths() {
		if v := matchFile(path, coreVersionRe); v != "" {
			return v, path
		}
	}
	return "", ""
}

// brewRoots are the Homebrew prefixes to look under. $HOMEBREW_PREFIX
// wins, because a user whose Homebrew is not in a conventional place
// means it — and because that is the variable brew itself exports, and
// the one koi's own shellenv (#44) sets. Without this the lookup is
// blind to every custom prefix, which on Linux is not the rare case.
func brewRoots() []string {
	if p := os.Getenv("HOMEBREW_PREFIX"); p != "" {
		return append([]string{p}, brewPrefixes...)
	}
	return brewPrefixes
}

// brewAPICachePaths are where Homebrew leaves the core formula metadata
// it downloaded, per platform. HOMEBREW_CACHE wins when set, because a
// user who moved the cache means it.
func brewAPICachePaths() []string {
	var roots []string
	if c := os.Getenv("HOMEBREW_CACHE"); c != "" {
		roots = append(roots, c)
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots,
			filepath.Join(home, "Library", "Caches", "Homebrew"), // macOS
			filepath.Join(home, ".cache", "Homebrew"),            // linux
		)
	}
	paths := make([]string, 0, len(roots))
	for _, r := range roots {
		paths = append(paths, filepath.Join(r, "api", "formula", "koi.json"))
	}
	return paths
}

// matchFile reads a file and returns the first capture, or "". Bounded:
// these are small metadata files, and a shell must not read an arbitrary
// number of megabytes because something on disk had the right name.
func matchFile(path string, re *regexp.Regexp) string {
	f, err := os.Open(path) //nolint:gosec // package-manager metadata, path built from fixed roots
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 512*1024)
	n, _ := io.ReadFull(f, buf)
	if n == 0 {
		return ""
	}
	if m := re.FindSubmatch(buf[:n]); m != nil {
		return string(m[1])
	}
	return ""
}

// newerVersion reports whether latest is a later release than current.
//
// Deliberately conservative: anything it cannot parse with confidence
// answers false. A wrong "an update is available" is a nag about nothing
// and a small lie about the user's own machine, while a missed notice
// costs one cycle — so every ambiguity resolves toward silence. A "dev"
// build never notifies, since it is by definition not a release.
func newerVersion(current, latest string) bool {
	if current == "" || current == "dev" || latest == "" {
		return false
	}
	// Asymmetric on purpose. A suffix on *current* is trimmed, so someone
	// running 0.1.0-rc1 still hears about 0.2.0. A suffix on *latest*
	// means the local metadata is offering a pre-release, and koi does
	// not advertise those — the notice exists to move people onto
	// something more finished than what they have, which a release
	// candidate is not.
	if isPreRelease(latest) {
		return false
	}
	cur, ok := parseVersion(current)
	if !ok {
		return false
	}
	next, ok := parseVersion(latest)
	if !ok {
		return false
	}
	for i := range next {
		if next[i] != cur[i] {
			return next[i] > cur[i]
		}
	}
	return false
}

// parseVersion takes the leading numeric dotted triple, ignoring a "v"
// prefix and any pre-release or build suffix.
func parseVersion(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// isPreRelease reports whether a version carries a pre-release or build
// suffix, in the semver sense: anything after a "-" or "+".
func isPreRelease(s string) bool {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	return strings.ContainsAny(s, "-+")
}
