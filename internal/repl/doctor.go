package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/blairham/gish/internal/envtrust"
	"github.com/blairham/gish/internal/history"
	"github.com/blairham/gish/internal/p10k"
	"github.com/blairham/gish/internal/pluginhost"
	"github.com/blairham/gish/internal/remote"
	"github.com/blairham/gish/internal/sandbox"
	"github.com/blairham/gish/internal/tools"
	"github.com/blairham/gish/internal/ui"
)

// The doctor command (#67): one command that checks the moving parts,
// says what's wrong, and names the exact fix — so a broken setup is
// self-serviceable. Advisory only: doctor never mutates state. It runs
// as a CallHandler builtin like config, which also makes it reachable
// from a working shell via `gish -c doctor` when gish itself won't
// start cleanly.

type checkStatus int

const (
	checkOK checkStatus = iota
	checkWarn
	checkFail
)

var statusMark = map[checkStatus]string{checkOK: "✔", checkWarn: "⚠", checkFail: "✘"}

// checkResult is one doctor line: a verdict, a one-line detail, and —
// for problems — the command (or action) that fixes it.
type checkResult struct {
	status checkStatus
	label  string
	detail string
	fix    string
}

// doctorCallHandler intercepts `doctor` before execution, config-style.
func doctorCallHandler(next interp.CallHandlerFunc) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if args[0] != "doctor" {
			return next(ctx, args)
		}
		return runDoctor(interp.HandlerCtx(ctx)), nil
	}
}

func runDoctor(hc interp.HandlerContext) []string {
	results := []checkResult{
		checkRC(),
		checkTheme(hc.Env),
		checkLint(hc.Env),
		checkHistory(),
		checkPlugins(),
		checkEnvTrust(),
		checkTools(hc),
		checkSandbox(),
		checkSemanticMarks(hc),
		checkRemoteSSH(hc),
		checkTerminal(),
	}

	style := ui.Styles(ui.Enabled(hc.Stdout))
	markStyle := map[checkStatus]func(...string) string{
		checkOK: style.OK.Render, checkWarn: style.Warn.Render, checkFail: style.Fail.Render,
	}
	healthy := true
	for _, r := range results {
		fmt.Fprintf(hc.Stdout, "%s %s %s\n",
			markStyle[r.status](statusMark[r.status]),
			style.Bold.Render(fmt.Sprintf("%-9s", r.label)), r.detail)
		if r.fix != "" {
			fmt.Fprintf(hc.Stdout, "  %-9s %s\n", "", style.Dim.Render("fix: "+r.fix))
		}
		if r.status == checkFail {
			healthy = false
		}
	}
	if !healthy {
		return []string{"false"}
	}
	return []string{"true"}
}

// checkRC reports which rc file is active and whether it parses; a
// broken rc file never stops the shell, but it silently skips the
// user's setup — doctor says where and why.
func checkRC() checkResult {
	path := rcPath()
	if path == "" {
		return checkResult{checkOK, "rc", "no rc file — defaults apply (config <setting> <value> creates one)", ""}
	}
	display := displayPath(path)
	f, err := os.Open(path) //nolint:gosec // the user's own rc path
	if os.IsNotExist(err) { // $GISH_RC may point at a file not created yet
		return checkResult{checkOK, "rc", "no rc file — defaults apply (config <setting> <value> creates one)", ""}
	}
	if err != nil {
		return checkResult{
			checkFail, "rc", fmt.Sprintf("%s: %v", display, err),
			"make the file readable, or point $GISH_RC elsewhere",
		}
	}
	defer f.Close()
	if _, err := syntax.NewParser().Parse(f, path); err != nil {
		return checkResult{
			checkFail, "rc", fmt.Sprintf("does not parse: %v", err),
			fmt.Sprintf("edit %s — the shell starts anyway but skips it", display),
		}
	}
	return checkResult{checkOK, "rc", display + " parses cleanly", ""}
}

// checkTheme validates the whole GISH_THEME_* surface. The prompt
// already degrades on bad values; doctor says why it degraded.
func checkTheme(env expand.Environ) checkResult {
	theme := env.Get("GISH_THEME").String()
	if theme == "" {
		theme = "plain"
	}
	if !slices.Contains([]string{"plain", "p10k", "gish", "starship"}, theme) {
		return checkResult{
			checkWarn, "theme",
			fmt.Sprintf("GISH_THEME=%q is not built-in — a plugin theme may claim it; otherwise the native theme renders", theme),
			"config theme p10k   (or plain | gish | starship, or install the plugin that serves it)",
		}
	}
	if theme == "p10k" {
		if preset := env.Get("GISH_P10K_PRESET").String(); preset != "" && p10k.Preset(preset) == nil {
			return checkResult{
				checkWarn, "theme",
				fmt.Sprintf("GISH_P10K_PRESET=%q is not a preset — rendering %s instead", preset, p10k.DefaultPreset),
				"p10k configure   (presets: " + strings.Join(p10k.Presets(), " | ") + ")",
			}
		}
	}
	if theme == "starship" {
		if _, err := exec.LookPath("starship"); err != nil {
			return checkResult{
				checkWarn, "theme",
				"GISH_THEME=starship but no starship binary on PATH — using the native theme",
				"install starship, or config theme p10k",
			}
		}
	}

	var problems []string
	for id := range strings.FieldsSeq(env.Get("GISH_THEME_SEGMENTS").String()) {
		if !segmentIDRe.MatchString(id) {
			problems = append(problems, fmt.Sprintf("segment id %q", id))
		}
	}
	env.Each(func(name string, vr expand.Variable) bool {
		if id, ok := strings.CutPrefix(name, "GISH_THEME_COLOR_"); ok {
			if v := vr.String(); v != "" {
				if _, valid := colorSGR(v); !valid {
					problems = append(problems, fmt.Sprintf("color %s=%q", id, v))
				}
			}
		}
		return true
	})
	if v := env.Get("GISH_THEME_LINES").String(); v != "" && v != "1" && v != "2" {
		problems = append(problems, fmt.Sprintf("GISH_THEME_LINES=%q (want 1 or 2)", v))
	}
	if v := env.Get("GISH_THEME_SEP").String(); v != "" && v != "plain" && v != "powerline" {
		problems = append(problems, fmt.Sprintf("GISH_THEME_SEP=%q (want plain or powerline)", v))
	}
	if v := env.Get("GISH_THEME_FRAME").String(); v != "" && v != "on" && v != "off" {
		problems = append(problems, fmt.Sprintf("GISH_THEME_FRAME=%q (want on or off)", v))
	}
	if len(problems) > 0 {
		return checkResult{
			checkWarn, "theme",
			"ignored (fell back to defaults): " + strings.Join(problems, ", "),
			"config theme.segments / theme.color.<id> / theme.lines / theme.sep to rewrite them",
		}
	}
	return checkResult{checkOK, "theme", theme, ""}
}

// checkLint flags the half-enabled state: the Enter-time multi-line
// pass wants shellcheck, and GISH_LINT=on quietly skips it when the
// binary is missing.
func checkLint(env expand.Environ) checkResult {
	mode := env.Get("GISH_LINT").String()
	switch mode {
	case "off", "native":
		return checkResult{checkOK, "lint", mode, ""}
	}
	if _, err := exec.LookPath("shellcheck"); err != nil {
		return checkResult{
			checkWarn, "lint",
			"GISH_LINT=on but shellcheck is not on PATH — the Enter-time multi-line pass is inactive",
			"install shellcheck, or config lint native",
		}
	}
	return checkResult{checkOK, "lint", "on (shellcheck found)", ""}
}

// checkHistory verifies the local file — the shell owns history, so
// this file working means history works with zero plugins installed.
func checkHistory() checkResult {
	path, err := history.DefaultPath()
	if err != nil {
		return checkResult{checkFail, "history", err.Error(), ""}
	}
	display := displayPath(path)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return checkResult{checkOK, "history", "not created yet — the first command writes " + display, ""}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the user's own history path
	if err != nil {
		return checkResult{
			checkFail, "history", display + " is not writable — new commands will not be recorded",
			"fix permissions on " + display,
		}
	}
	f.Close()
	if line := lastLine(path); line != "" && !json.Valid([]byte(line)) {
		return checkResult{
			checkWarn, "history",
			display + " has an unparsable tail entry — lookups skip it, new entries still append", "",
		}
	}
	return checkResult{checkOK, "history", display + " is writable", ""}
}

// lastLine returns the file's last non-empty line ("" on any trouble —
// the caller treats unreadable as fine; writability is checked apart).
func lastLine(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // the user's own history path
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// checkPlugins looks at the tier-2 discovery directory: every entry
// should be executable, or the host silently never finds it.
func checkPlugins() checkResult {
	dir, err := pluginhost.DefaultDir()
	if err != nil {
		return checkResult{checkFail, "plugins", err.Error(), ""}
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return checkResult{checkOK, "plugins", "none installed (no " + displayPath(dir) + ")", ""}
	}
	if err != nil {
		return checkResult{checkFail, "plugins", fmt.Sprintf("%s: %v", displayPath(dir), err), ""}
	}
	var executable int
	var stuck []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, ferr := e.Info()
		if ferr != nil {
			continue
		}
		if pluginhost.ExecutablePlugin(fi) {
			executable++
		} else {
			stuck = append(stuck, e.Name())
		}
	}
	if len(stuck) > 0 {
		fix := "chmod +x " + filepath.Join(displayPath(dir), stuck[0])
		if runtime.GOOS == "windows" {
			fix = "give it an executable extension (.exe): " + filepath.Join(displayPath(dir), stuck[0])
		}
		return checkResult{
			checkWarn, "plugins",
			fmt.Sprintf("%d not executable (invisible to discovery): %s", len(stuck), strings.Join(stuck, ", ")),
			fix,
		}
	}
	return checkResult{checkOK, "plugins", fmt.Sprintf("%d in %s", executable, displayPath(dir)), ""}
}

// checkEnvTrust verifies the env-diff allow list (#12): a corrupt file
// disables env plugins for the session rather than resetting trust.
func checkEnvTrust() checkResult {
	path, err := envtrust.DefaultPath()
	if err != nil {
		return checkResult{checkFail, "env-trust", err.Error(), ""}
	}
	display := displayPath(path)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return checkResult{checkOK, "env-trust", "no env diffs allowed yet — `trust` records them", ""}
	}
	store, err := envtrust.Open(path)
	if err != nil {
		return checkResult{
			checkFail, "env-trust",
			display + " does not parse — env plugins are disabled this session",
			"fix or remove " + display + " (recorded allows are lost either way)",
		}
	}
	return checkResult{checkOK, "env-trust", fmt.Sprintf("%d allowed director(ies) in %s", len(store.Entries()), display), ""}
}

// checkTools reports what the directory's pins resolve to (#77): a
// pinned-but-not-installed version is the one silent gap worth naming.
func checkTools(hc interp.HandlerContext) checkResult {
	if hc.Env.Get("GISH_TOOLS").String() == "off" {
		return checkResult{checkOK, "tools", "off (GISH_TOOLS)", ""}
	}
	res := tools.Resolve(hc.Dir, tools.InstallRoots())
	if res.File == "" {
		return checkResult{checkOK, "tools", "no .tool-versions in scope", ""}
	}
	if len(res.Missing) > 0 {
		var names []string
		for _, pin := range res.Missing {
			names = append(names, pin.Tool+" "+pin.Versions[0])
		}
		return checkResult{
			checkWarn, "tools",
			fmt.Sprintf("%s pins versions that are not installed: %s", displayPath(res.File), strings.Join(names, ", ")),
			"asdf install " + names[0],
		}
	}
	return checkResult{
		checkOK, "tools",
		fmt.Sprintf("%d bin dir(s) active from %s", len(res.Bins), displayPath(res.File)), "",
	}
}

// checkSandbox reports the enforcement ceiling (#21) — a sandbox that
// silently cannot enforce is worse than none, so doctor says so.
func checkSandbox() checkResult {
	avail := sandbox.Available()
	for _, degraded := range []string{"unenforced", "unavailable", "not supported"} {
		if strings.HasPrefix(avail, degraded) {
			return checkResult{checkWarn, "sandbox", avail, ""}
		}
	}
	return checkResult{checkOK, "sandbox", avail, ""}
}

// checkSemanticMarks reports OSC 133 block-navigation support (#99):
// gish emits the marks, and this says whether the terminal is one
// known to act on them.
func checkSemanticMarks(hc interp.HandlerContext) checkResult {
	if hc.Env.Get("GISH_SEMANTIC_MARKS").String() == "off" {
		return checkResult{checkOK, "blocks", "semantic marks off (GISH_SEMANTIC_MARKS)", ""}
	}
	detail, known := doctorSemanticMarks()
	if !known {
		return checkResult{checkOK, "blocks", "OSC 133 marks " + detail, ""}
	}
	return checkResult{checkOK, "blocks", detail, ""}
}

// checkRemoteSSH reports what `gish ssh` (#98) would be able to do from
// here. The one finding that actually bites is a cgo-linked binary:
// `uname -sm` says "linux x86_64" and nothing about glibc versus musl,
// so such a build lands on Alpine and fails with an error that looks
// like the file is missing.
func checkRemoteSSH(hc interp.HandlerContext) checkResult {
	if os.Getenv("GISH_REMOTE_SESSION") != "" {
		return checkResult{checkOK, "ssh", "this session was brought here by `gish ssh`", ""}
	}
	mode := hc.Env.Get("GISH_SSH_BRING").String()
	if mode == "" {
		mode = "ask"
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return checkResult{checkWarn, "ssh", "no ssh binary — `gish ssh` unavailable", "install openssh-client"}
	}
	if ok, detail := remote.StaticCheck(); !ok {
		return checkResult{
			checkWarn, "ssh", detail,
			"go build -ldflags='-s -w' with CGO_ENABLED=0, or install a release build",
		}
	}
	return checkResult{checkOK, "ssh", fmt.Sprintf("`gish ssh` ready, bring=%s (static build)", mode), ""}
}

// checkTerminal explains the environment-driven degradations: they are
// working as designed, but they look like breakage.
func checkTerminal() checkResult {
	term := os.Getenv("TERM")
	switch {
	case term == "":
		return checkResult{
			checkWarn, "terminal", "TERM is empty — line editor and themes disabled",
			"export TERM (xterm-256color is a safe default)",
		}
	case term == "dumb":
		return checkResult{checkOK, "terminal", "TERM=dumb — naked prompt by design", ""}
	case os.Getenv("NO_COLOR") != "":
		return checkResult{checkOK, "terminal", term + " (NO_COLOR set — naked prompt by design)", ""}
	}
	return checkResult{checkOK, "terminal", term, ""}
}

// displayPath tildifies for humans; the raw path on any trouble.
func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return tildify(path, home)
}
