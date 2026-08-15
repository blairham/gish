package repl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// The config command: first-run ergonomics for a shell that starts
// naked. `config theme starship` flips the setting live in the session
// AND persists it to the rc file, so nobody has to know which file
// GISH_THEME lives in. Groundwork for the #28 configure wizard.
//
// Implemented as a CallHandler rewrite (the zi pattern): validation and
// rc persistence happen here in the handler, and the live effect is the
// rewritten `eval KEY=value` the interpreter runs in the session itself
// — no reaching into runner internals.

// configSetting describes one tunable: its shell variable, its value
// set (empty allowed = free-form), and a one-line description.
type configSetting struct {
	name    string
	varName string
	allowed []string
	desc    string
}

// configSettings is ordered for display.
var configSettings = []configSetting{
	{"theme", "GISH_THEME", []string{"plain", "p10k", "starship"}, "prompt theme — plain is the naked default"},
	{"lint", "GISH_LINT", []string{"on", "native", "off"}, "footgun diagnostics — native skips shellcheck"},
	{"prompt", "GISH_PROMPT", nil, "manual prompt escapes — beats any theme"},
}

const configUsage = `usage: config [setting [value]]

  config                 show all settings and their current values
  config theme           show one setting
  config theme starship  set it: live now, and saved to the rc file

settings:
  theme   plain | p10k | starship  (GISH_THEME)
  lint    on | native | off        (GISH_LINT)
  prompt  escape string            (GISH_PROMPT)`

// configCallHandler intercepts `config` before execution, zi-style.
func configCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "config" {
			return next(ctx, args)
		}
		return runConfig(interp.HandlerCtx(ctx), args[1:]), nil
	}
}

func runConfig(hc interp.HandlerContext, args []string) []string {
	fail := func(err error) []string {
		fmt.Fprintln(hc.Stderr, "config:", err)
		return []string{"false"}
	}
	if len(args) == 0 {
		for _, s := range configSettings {
			fmt.Fprintf(hc.Stdout, "%-8s %-10q %s\n", s.name, hc.Env.Get(s.varName).String(), s.desc)
		}
		return []string{"true"}
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(hc.Stdout, configUsage)
		return []string{"true"}
	}

	idx := slices.IndexFunc(configSettings, func(s configSetting) bool { return s.name == args[0] })
	if idx == -1 {
		return fail(fmt.Errorf("unknown setting %q\n%s", args[0], configUsage))
	}
	setting := configSettings[idx]

	switch len(args) {
	case 1:
		fmt.Fprintf(hc.Stdout, "%s = %q (%s)\n", setting.name, hc.Env.Get(setting.varName).String(), setting.varName)
		return []string{"true"}
	case 2: // set
	default:
		return fail(fmt.Errorf("usage: config %s <value>", setting.name))
	}

	value := args[1]
	if len(setting.allowed) > 0 && !slices.Contains(setting.allowed, value) {
		return fail(fmt.Errorf("%s must be one of: %s", setting.name, strings.Join(setting.allowed, " | ")))
	}
	quoted, err := syntax.Quote(value, syntax.LangBash)
	if err != nil {
		return fail(err)
	}
	path, err := writeRCSetting(setting.varName, quoted)
	if err != nil {
		return fail(err)
	}
	display := path
	if home, herr := os.UserHomeDir(); herr == nil {
		display = tildify(path, home)
	}
	fmt.Fprintf(hc.Stdout, "%s = %q — saved to %s\n", setting.name, value, display)
	// The interpreter runs the assignment in the live session.
	return []string{"eval", setting.varName + "=" + quoted}
}

// rcWritePath is where config persists: $GISH_RC when set, else the
// first existing rc file, else the XDG location (created on write).
func rcWritePath() (string, error) {
	if p := os.Getenv("GISH_RC"); p != "" {
		return p, nil
	}
	if p := rcPath(); p != "" {
		return p, nil
	}
	confHome := os.Getenv("XDG_CONFIG_HOME")
	if confHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		confHome = filepath.Join(home, ".config")
	}
	return filepath.Join(confHome, "gish", "gishrc"), nil
}

// writeRCSetting rewrites every top-level assignment of varName in the
// rc file to the (already shell-quoted) value, or appends one; a user's
// `export ` prefix is kept. The file is created on first use.
func writeRCSetting(varName, quoted string) (string, error) {
	path, err := rcWritePath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	}

	assignment := varName + "=" + quoted
	lines := strings.Split(string(data), "\n")
	replaced := false
	for i, l := range lines {
		bare := strings.TrimPrefix(strings.TrimSpace(l), "export ")
		if !strings.HasPrefix(bare, varName+"=") {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(l), "export ") {
			lines[i] = "export " + assignment
		} else {
			lines[i] = assignment
		}
		replaced = true
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if !replaced {
		lines = append(lines, assignment)
	}
	out := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil { //nolint:gosec // an rc file is not a secret
		return "", err
	}
	return path, nil
}
