package repl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// starshipBudget bounds one whole-prompt render. Starship typically
// answers in 20–50ms; a miss serves the previous prompt — the gish rule.
const starshipBudget = 150 * time.Millisecond

// starshipTheme renders the prompt through the user's starship binary
// (#45): their exact starship.toml, unchanged. This is the subprocess
// flavor of the #30 ThemeProvider contract.
type starshipTheme struct {
	once   sync.Once
	path   string
	warned bool

	mu       sync.Mutex
	lastMain string
	lastCont string
}

var starship starshipTheme

// render produces the prompt pair via `starship prompt` (flag contract
// verified against starship 1.26). Failures fall back to the last good
// render, then to the native theme.
func (s *starshipTheme) render(info promptInfo, width int) (string, string, bool) {
	s.once.Do(func() {
		s.path, _ = exec.LookPath("starship") //nolint:errcheck // absence handled below
	})
	if s.path == "" {
		if !s.warned {
			s.warned = true
			fmt.Fprintln(os.Stderr, "gish: GISH_THEME=starship but starship is not installed; using the native theme")
		}
		return "", "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), starshipBudget)
	defer cancel()
	args := []string{
		"prompt",
		"--status=" + strconv.Itoa(info.exitCode),
		"--cmd-duration=" + strconv.FormatInt(info.duration.Milliseconds(), 10),
		"--jobs=" + strconv.Itoa(info.jobs),
		"--terminal-width=" + strconv.Itoa(width),
	}
	cmd := exec.CommandContext(ctx, s.path, args...)
	cmd.Dir = info.dir
	cmd.Env = append(os.Environ(), "STARSHIP_SHELL=gish")
	out, err := cmd.Output()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || len(out) == 0 {
		if s.lastMain != "" {
			return s.lastMain, s.lastCont, true // stale beats blocking
		}
		return "", "", false
	}
	s.lastMain = string(out)

	if s.lastCont == "" {
		cctx, ccancel := context.WithTimeout(context.Background(), starshipBudget)
		cont := exec.CommandContext(cctx, s.path, "prompt", "--continuation")
		cont.Env = cmd.Env
		if cout, cerr := cont.Output(); cerr == nil && len(cout) > 0 {
			s.lastCont = string(cout)
		} else {
			s.lastCont = contPrompt
		}
		ccancel()
	}
	return s.lastMain, s.lastCont, true
}

// starshipHint prints a one-time nudge when the user clearly uses
// starship but hasn't told gish (never auto-switch — #45 rule).
func starshipHint(themeSet, promptSet bool) {
	if themeSet || promptSet {
		return
	}
	if _, err := exec.LookPath("starship"); err != nil {
		return
	}
	confHome := os.Getenv("XDG_CONFIG_HOME")
	if confHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		confHome = filepath.Join(home, ".config")
	}
	if _, err := os.Stat(filepath.Join(confHome, "starship.toml")); err == nil {
		fmt.Fprintln(os.Stderr, "gish: starship detected — set GISH_THEME=starship in your gishrc to use it")
	}
}
