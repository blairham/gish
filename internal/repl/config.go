package repl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

per-segment theme keys (#28):
  config theme.segments 'dir git exit'  pick and order themed segments
  config theme.git off                  toggle one segment on|off
  config theme.color.dir cyan           color override for one segment

settings:
  theme   plain | p10k | starship  (GISH_THEME)
  lint    on | native | off        (GISH_LINT)
  prompt  escape string            (GISH_PROMPT)
  theme.segments    ordered ids — built-ins dir git pins jobs duration
                    exit, plus any plugin segment id  (GISH_THEME_SEGMENTS)
  theme.color.<id>  color name, raw SGR params, or default
                    (GISH_THEME_COLOR_<ID>)`

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
	if strings.HasPrefix(args[0], "theme.") {
		return runThemeConfig(hc, fail, args)
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
	return persistConfig(hc, fail, setting.name, setting.varName, value)
}

// persistConfig is the shared tail of every set: quote, save to the rc
// file, announce, and hand the interpreter the live assignment.
func persistConfig(hc interp.HandlerContext, fail func(error) []string, name, varName, value string) []string {
	quoted, err := syntax.Quote(value, syntax.LangBash)
	if err != nil {
		return fail(err)
	}
	path, err := writeRCSetting(varName, quoted)
	if err != nil {
		return fail(err)
	}
	display := path
	if home, herr := os.UserHomeDir(); herr == nil {
		display = tildify(path, home)
	}
	fmt.Fprintf(hc.Stdout, "%s = %q — saved to %s\n", name, value, display)
	// The interpreter runs the assignment in the live session.
	return []string{"eval", varName + "=" + quoted}
}

var segmentIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// runThemeConfig handles the dotted per-segment theme keys (#28):
//
//	config theme.segments 'dir git exit'  pick and order segments
//	config theme.git off                  toggle one segment
//	config theme.color.dir cyan           per-segment color override
func runThemeConfig(hc interp.HandlerContext, fail func(error) []string, args []string) []string {
	key := strings.TrimPrefix(args[0], "theme.")
	if len(args) == 1 {
		return showThemeKey(hc, key)
	}
	if len(args) > 2 {
		return fail(fmt.Errorf("usage: config %s <value>", args[0]))
	}
	value := args[1]

	switch {
	case key == "segments":
		ids := strings.Fields(value)
		if len(ids) == 0 {
			return fail(errors.New("theme.segments needs at least one segment id"))
		}
		for _, id := range ids {
			if !segmentIDRe.MatchString(id) {
				return fail(fmt.Errorf("bad segment id %q", id))
			}
		}
		return persistConfig(hc, fail, args[0], "GISH_THEME_SEGMENTS", strings.Join(ids, " "))

	case strings.HasPrefix(key, "color."):
		id := strings.TrimPrefix(key, "color.")
		if !segmentIDRe.MatchString(id) {
			return fail(fmt.Errorf("bad segment id %q", id))
		}
		if _, ok := colorSGR(value); !ok && value != "default" {
			return fail(fmt.Errorf(
				"bad color %q — a name (cyan, bright-red, dim, …), raw SGR params (38;5;208), or default", value))
		}
		if value == "default" {
			value = "" // an empty override falls back to the built-in color
		}
		return persistConfig(hc, fail, args[0], themeColorVar(id), value)

	default: // a segment id: on | off
		if !segmentIDRe.MatchString(key) {
			return fail(fmt.Errorf("bad segment id %q", key))
		}
		if value != "on" && value != "off" {
			return fail(fmt.Errorf("usage: config theme.%s on|off", key))
		}
		next, err := toggleSegment(currentSegments(hc), key, value == "on")
		if err != nil {
			return fail(err)
		}
		return persistConfig(hc, fail, args[0], "GISH_THEME_SEGMENTS", strings.Join(next, " "))
	}
}

// currentSegments is the session's effective segment list: the variable
// when set, the built-in default order otherwise.
func currentSegments(hc interp.HandlerContext) []string {
	if segments := strings.Fields(hc.Env.Get("GISH_THEME_SEGMENTS").String()); len(segments) > 0 {
		return segments
	}
	return defaultSegmentIDs()
}

// toggleSegment adds or removes one id. A built-in coming back on slots
// into its default-order position; plugin segments append at the end.
func toggleSegment(segments []string, id string, on bool) ([]string, error) {
	present := slices.Contains(segments, id)
	if !on {
		if !present {
			return segments, nil
		}
		if len(segments) == 1 {
			return nil, errors.New("cannot turn off the last segment")
		}
		return slices.DeleteFunc(slices.Clone(segments), func(s string) bool { return s == id }), nil
	}
	if present {
		return segments, nil
	}
	defaults := defaultSegmentIDs()
	rank := slices.Index(defaults, id)
	if rank == -1 {
		return append(slices.Clone(segments), id), nil
	}
	for i, s := range segments {
		if r := slices.Index(defaults, s); r > rank {
			return slices.Insert(slices.Clone(segments), i, id), nil
		}
	}
	return append(slices.Clone(segments), id), nil
}

// showThemeKey prints one dotted key's current value.
func showThemeKey(hc interp.HandlerContext, key string) []string {
	switch {
	case key == "segments":
		fmt.Fprintf(hc.Stdout, "theme.segments = %q (GISH_THEME_SEGMENTS)\n",
			strings.Join(currentSegments(hc), " "))
	case strings.HasPrefix(key, "color."):
		varName := themeColorVar(strings.TrimPrefix(key, "color."))
		fmt.Fprintf(hc.Stdout, "theme.%s = %q (%s)\n", key, hc.Env.Get(varName).String(), varName)
	default:
		state := "off"
		if slices.Contains(currentSegments(hc), key) {
			state = "on"
		}
		fmt.Fprintf(hc.Stdout, "theme.%s = %s (GISH_THEME_SEGMENTS)\n", key, state)
	}
	return []string{"true"}
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
