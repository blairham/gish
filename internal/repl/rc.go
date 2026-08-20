package repl

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/koi-shell/internal/shell/interp"
)

// rcPath resolves the interactive startup file. Precedence:
//
//  1. $KOI_RC — explicit override (and the testing seam)
//  2. $XDG_CONFIG_HOME/koi/koirc — XDG_CONFIG_HOME defaults to ~/.config
//  3. ~/.koirc — the classic location
//
// The first path that exists wins; none existing is not an error.
func rcPath() string {
	// An explicit override wins unconditionally, existing or not: it is an
	// instruction rather than a candidate, and config must create *that*
	// file rather than quietly writing somewhere else. Pinned by
	// TestRCPathPrecedence, which caught this being refactored away.
	if p := os.Getenv("KOI_RC"); p != "" {
		return p
	}
	for _, p := range defaultRCPaths() {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func defaultRCPaths() []string {
	var candidates []string
	confHome := os.Getenv("XDG_CONFIG_HOME")
	home, herr := os.UserHomeDir()
	if confHome == "" && herr == nil {
		confHome = filepath.Join(home, ".config")
	}
	if confHome != "" {
		candidates = append(candidates, filepath.Join(confHome, "koi", "koirc"))
	}
	if herr == nil {
		candidates = append(candidates, filepath.Join(home, ".koirc"))
	}
	return candidates
}

// shadowedRCs returns the rc files that exist and are being skipped
// because something above them in the precedence order exists too.
//
// This is the whole of #232. Nothing here misbehaves: rc resolution is
// first-hit-wins and documented, `config` writes to whichever file is
// already authoritative, and the theme correctly falls back when its
// variable is unset. The failure is that the information needed to
// understand any of that lives in three different places, and the one
// surface built to make configuration legible — doctor — reported two
// true and useless lines instead:
//
//	✔ rc        ~/.config/koi/koirc parses cleanly
//	✔ theme     plain
//
// A user who edited ~/.koirc has no reason to suspect a second rc file
// exists, so a ✔ actively argues their configuration is fine.
func shadowedRCs() []string {
	defaults := defaultRCPaths()
	existing := func(paths []string) []string {
		var out []string
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				out = append(out, p)
			}
		}
		return out
	}
	// With an explicit override in force, every default that exists is
	// being skipped — a user who sets KOI_RC and keeps a ~/.koirc is in
	// the same trap as anyone else.
	if os.Getenv("KOI_RC") != "" {
		return existing(defaults)
	}
	found := existing(defaults)
	if len(found) < 2 {
		return nil // the first is the active one; nothing below it exists
	}
	return found[1:]
}

// enableAliases turns on alias expansion for an interactive session.
//
// Without this, `alias` is a trap rather than a missing feature: the
// builtin exists and records the definition, so defining one looks like
// it worked, and using it reports "command not found". A switcher's rc
// is mostly aliases, so the first thing they try fails in the most
// confusing way available.
//
// Interactive only, which is bash's rule and worth keeping: expanding
// aliases in scripts changes what a script means depending on who runs
// it, and koi's -c path is pinned POSIX-clean by the compat suite. So
// runPlain (piped stdin) and RunReader (-c, script files) deliberately
// do not call this.
//
// It runs the shopt rather than reaching for an option constant because
// alias expansion is a shopt in the interpreter too; this is the same
// "make it true in the live runner" move `config` uses.
func enableAliases(ctx context.Context, runner *interp.Runner) {
	file, err := syntax.NewParser().Parse(strings.NewReader("shopt -s expand_aliases"), "koi")
	if err != nil {
		return
	}
	if err := runner.Run(ctx, file); err != nil {
		// Not fatal: a shell that cannot expand aliases is worse than one
		// that can, but far better than one that refuses to start.
		fmt.Fprintf(os.Stderr, "koi: enabling aliases: %v\n", err)
	}
}

// loadRC runs the rc file in the session runner, so functions, variables,
// cd, and exports persist into the interactive session. A missing file is
// normal; a broken one warns and the shell starts anyway — an rc error
// must never lock the user out of their shell.
// skipRC and skipProfile record bash's --norc and --noprofile (#531):
// a caller asking for a clean shell gets one, rather than a usage error
// and no shell at all.
var skipRC, skipProfile bool

// SkipStartupFiles records which startup files this invocation asked to
// skip. It is set once from argv, before anything is sourced.
func SkipStartupFiles(noRC, noProfile bool) {
	skipRC, skipProfile = noRC, noProfile
}

func loadRC(ctx context.Context, runner *interp.Runner) {
	if skipRC {
		return
	}
	// Adopted team fragments run first (#209), so the user's own rc has
	// the last word: anything they set beats anything a repo shipped,
	// by source order rather than by a merge policy. A missing
	// directory is the normal state — startup creates nothing.
	for _, fragment := range adoptedFragments() {
		runRCFile(ctx, runner, fragment)
	}
	path := rcPath()
	if path == "" {
		return
	}
	runRCFile(ctx, runner, path)
}

// runRCFile sources one startup file into the session runner, warning
// and continuing on every failure — a startup-file error must never
// lock the user out of their shell.
func runRCFile(ctx context.Context, runner *interp.Runner, path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "koi: %s: %v\n", path, err)
		return
	}
	defer f.Close()
	// Read as bash reads (#276). This is the case the issue was filed
	// about: an rc with one construct koi cannot read used to lose every
	// line of itself, so a single unsupported line at the bottom cost the
	// user their whole prompt, aliases and functions. Now it costs that
	// line and the ones after it, and the warning still names where.
	stmts, perr := interp.ParseAsRead(f, path)
	file := &syntax.File{Name: path, Stmts: stmts}
	if err := safely("running "+path, func() error { return runner.Run(ctx, file) }); err != nil {
		fmt.Fprintf(os.Stderr, "koi: %s: %v\n", path, err)
	}
	if perr != nil {
		fmt.Fprintf(os.Stderr, "koi: %s: %v\n", path, perr)
	}
}

// loadProfile sources login-shell startup files (#41): /etc/profile
// when present (macOS path_helper and distro PATH setup live there),
// then the first of $KOI_PROFILE, ~/.koi_profile, ~/.profile. Errors
// warn and continue — a broken profile must never lock the user out.
func loadProfile(ctx context.Context, runner *interp.Runner) {
	if skipProfile {
		return
	}
	paths := []string{"/etc/profile"}
	if p := os.Getenv("KOI_PROFILE"); p != "" {
		paths = append(paths, p)
	} else if home, err := os.UserHomeDir(); err == nil {
		for _, candidate := range []string{
			filepath.Join(home, ".koi_profile"),
			filepath.Join(home, ".profile"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				paths = append(paths, candidate)
				break
			}
		}
	}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue // missing files are normal
		}
		// Same rule as the rc above (#276): what parsed runs, and the
		// error is reported after it rather than instead of it. A broken
		// /etc/profile taking the machine's PATH setup down with it is
		// the version of this that is not the user's to fix.
		stmts, perr := interp.ParseAsRead(f, path)
		f.Close()
		file := &syntax.File{Name: path, Stmts: stmts}
		if rerr := safely("running "+path, func() error { return runner.Run(ctx, file) }); rerr != nil {
			fmt.Fprintf(os.Stderr, "koi: %s: %v\n", path, rerr)
		}
		if perr != nil {
			fmt.Fprintf(os.Stderr, "koi: %s: %v\n", path, perr)
		}
	}
}

// shellVar reads a scalar setting from the runner: shell variables
// first (an rc assignment or a live `config` change wins), then the
// inherited environment, so `KOI_THEME=p10k koi` works the way
// every other env-configured program does. runner.Vars holds only
// variables the session set; inherited ones live in runner.Env.
func shellVar(runner *interp.Runner, name, fallback string) string {
	if v, ok := runner.Vars[name]; ok {
		if s := v.String(); s != "" {
			return s
		}
	}
	if runner.Env != nil {
		if v := runner.Env.Get(name); v.IsSet() {
			if s := v.String(); s != "" {
				return s
			}
		}
	}
	return fallback
}

// promptInfo carries the per-render state the prompt escapes draw from.
// The static fields are computed once per session.
type promptInfo struct {
	username string
	host     string
	home     string
	dir      string
	exitCode int
	duration time.Duration // last command's wall time
	jobs     int           // filed jobs (running or stopped)
	width    int           // terminal width, for whole-prompt renderers
	// segment resolves %p{id} escapes (nil renders empty): tier-2
	// prompt plugins, budget-bounded, stale on miss.
	segment func(id string) string
}

// newPromptInfo resolves the session-static fields, tolerating failures
// (an escape just renders empty).
func newPromptInfo() promptInfo {
	var info promptInfo
	if u, err := user.Current(); err == nil {
		info.username = u.Username
	}
	if h, err := os.Hostname(); err == nil {
		info.host, _, _ = strings.Cut(h, ".")
	}
	if home, err := os.UserHomeDir(); err == nil {
		info.home = home
	}
	return info
}

// tildify abbreviates home to ~ at the start of dir.
func tildify(dir, home string) string {
	if home == "" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(dir, home+string(os.PathSeparator)); ok {
		return "~" + string(os.PathSeparator) + rest
	}
	return dir
}
