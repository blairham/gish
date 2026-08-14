package repl

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// rcPath resolves the interactive startup file. Precedence:
//
//  1. $GISH_RC — explicit override (and the testing seam)
//  2. $XDG_CONFIG_HOME/gish/gishrc — XDG_CONFIG_HOME defaults to ~/.config
//  3. ~/.gishrc — the classic location
//
// The first path that exists wins; none existing is not an error.
func rcPath() string {
	if p := os.Getenv("GISH_RC"); p != "" {
		return p
	}
	var candidates []string
	confHome := os.Getenv("XDG_CONFIG_HOME")
	home, herr := os.UserHomeDir()
	if confHome == "" && herr == nil {
		confHome = filepath.Join(home, ".config")
	}
	if confHome != "" {
		candidates = append(candidates, filepath.Join(confHome, "gish", "gishrc"))
	}
	if herr == nil {
		candidates = append(candidates, filepath.Join(home, ".gishrc"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// loadRC runs the rc file in the session runner, so functions, variables,
// cd, and exports persist into the interactive session. A missing file is
// normal; a broken one warns and the shell starts anyway — an rc error
// must never lock the user out of their shell.
func loadRC(ctx context.Context, runner *interp.Runner) {
	path := rcPath()
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gish: %s: %v\n", path, err)
		return
	}
	defer f.Close()
	file, err := syntax.NewParser().Parse(f, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gish: %s: %v\n", path, err)
		return
	}
	if err := runner.Run(ctx, file); err != nil {
		fmt.Fprintf(os.Stderr, "gish: %s: %v\n", path, err)
	}
}

// shellVar reads a scalar shell variable from the runner, falling back
// when unset or empty.
func shellVar(runner *interp.Runner, name, fallback string) string {
	if v, ok := runner.Vars[name]; ok {
		if s := v.String(); s != "" {
			return s
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

// expandPrompt renders the minimal zsh-style escape set. This is a
// deliberate stopgap: the real prompt engine (plugin segments, styling)
// arrives with M3 — see docs/plugins.md.
//
//	%u  username         %w  cwd, ~-abbreviated
//	%h  hostname         %W  cwd basename (~ at home)
//	%?  last exit code   %%  literal %
//	%p{id}  tier-2 prompt segment (e.g. %p{git}); empty when no plugin
//	        serves the id or the render misses its budget with no cache
//
// Unknown escapes pass through verbatim so future additions aren't
// breaking.
func expandPrompt(format string, info promptInfo) string {
	var b strings.Builder
	runes := []rune(format)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '%' || i+1 >= len(runes) {
			b.WriteRune(runes[i])
			continue
		}
		i++
		switch runes[i] {
		case '%':
			b.WriteByte('%')
		case 'u':
			b.WriteString(info.username)
		case 'h':
			b.WriteString(info.host)
		case 'w':
			b.WriteString(tildify(info.dir, info.home))
		case 'W':
			if info.home != "" && info.dir == info.home {
				b.WriteByte('~')
			} else {
				b.WriteString(filepath.Base(info.dir))
			}
		case '?':
			b.WriteString(strconv.Itoa(info.exitCode))
		case 'p':
			id, next, ok := bracedArg(runes, i)
			if !ok {
				b.WriteString("%p")
				continue
			}
			i = next
			if info.segment != nil {
				b.WriteString(info.segment(id))
			}
		default:
			b.WriteByte('%')
			b.WriteRune(runes[i])
		}
	}
	return b.String()
}

// bracedArg parses {arg} starting right after the escape rune at i;
// returns the arg and the index of the closing brace.
func bracedArg(runes []rune, i int) (arg string, next int, ok bool) {
	if i+1 >= len(runes) || runes[i+1] != '{' {
		return "", i, false
	}
	for j := i + 2; j < len(runes); j++ {
		if runes[j] == '}' {
			return string(runes[i+2 : j]), j, true
		}
	}
	return "", i, false
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
