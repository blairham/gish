package compat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The source gate (#161 scope B).
//
// The named breakers, recurring across a decade of abandonment
// accounts: nvm, conda, rvm, rbenv, virtualenv's activate,
// virtualenvwrapper, direnv. Someone switches shells, sources the file
// their whole toolchain depends on, and it does not work — so they
// switch back the same afternoon.
//
// These are run **unmodified**, as the user has them installed, and
// differentially: the same load-and-probe script through real bash and
// through gish. A tool that is not installed is reported as absent
// rather than skipped silently, because "we pass every case we ran" is
// only meaningful alongside what was not run.

// SourceCase loads one real init script and probes what it defined.
type SourceCase struct {
	Name       string
	Provenance string
	// Locate returns the shell snippet that loads the tool and whether
	// it is present on this machine. It may create files (a venv), so
	// it takes a scratch directory.
	Locate func(scratch string) (string, bool)
	// Probe runs after the load; its output is what gets compared.
	// Probes must avoid version strings and paths that differ between
	// runs — this is a compatibility gate, not a golden file.
	Probe string
}

// SourceCorpus is the published gate.
var SourceCorpus = []SourceCase{
	{
		Name:       "nvm",
		Provenance: "the single most-named breaker in the churn accounts",
		Locate:     locateFile([]string{"$NVM_DIR/nvm.sh", "$HOME/.nvm/nvm.sh", "/usr/local/opt/nvm/nvm.sh", "/opt/homebrew/opt/nvm/nvm.sh"}),
		// nvm is a shell function, so `command -v` is the honest probe:
		// it proves the function exists in this shell, which is exactly
		// what fails when a shell cannot run the script.
		Probe: `command -v nvm >/dev/null && echo nvm-loaded; nvm --help >/dev/null 2>&1 && echo nvm-runs`,
	},
	{
		Name:       "rbenv init",
		Provenance: "named in the abandonment accounts alongside nvm",
		Locate:     locateEval("rbenv", "rbenv init - bash"),
		Probe:      `command -v rbenv >/dev/null && echo rbenv-loaded; rbenv version-name >/dev/null 2>&1 && echo rbenv-runs`,
	},
	{
		Name:       "pyenv init",
		Provenance: "same class as rbenv; ships a bash-specific init",
		Locate:     locateEval("pyenv", "pyenv init - bash"),
		Probe:      `command -v pyenv >/dev/null && echo pyenv-loaded`,
	},
	{
		Name:       "conda shell hook",
		Provenance: "conda init's output is a named breaker",
		Locate:     locateEval("conda", "conda shell.bash hook"),
		Probe:      `command -v conda >/dev/null && echo conda-loaded; conda config --show-sources >/dev/null 2>&1 && echo conda-runs`,
	},
	{
		Name:       "rvm",
		Provenance: "the Ruby half of the nvm story",
		Locate:     locateFile([]string{"$HOME/.rvm/scripts/rvm", "/usr/local/rvm/scripts/rvm"}),
		Probe:      `command -v rvm >/dev/null && echo rvm-loaded`,
	},
	{
		Name:       "virtualenv activate",
		Provenance: "`.venv/bin/activate` — the most-sourced script in Python work",
		Locate:     locateVenv,
		Probe:      `echo "${VIRTUAL_ENV:+venv-active}"; command -v python >/dev/null && echo python-on-path; deactivate; echo "after=${VIRTUAL_ENV:-unset}"`,
	},
	{
		Name:       "virtualenvwrapper",
		Provenance: "named in the corpus; defines a dozen functions at source time",
		Locate:     locateFile([]string{"$HOME/.local/bin/virtualenvwrapper.sh", "/usr/local/bin/virtualenvwrapper.sh", "/usr/bin/virtualenvwrapper.sh"}),
		Probe:      `command -v mkvirtualenv >/dev/null && echo wrapper-loaded`,
	},
	{
		Name:       "direnv hook",
		Provenance: "`eval \"$(direnv hook bash)\"` is in a great many rc files",
		Locate:     locateEval("direnv", "direnv hook bash"),
		Probe:      `[ -n "${PROMPT_COMMAND:-}" ] && echo hook-installed; command -v _direnv_hook >/dev/null && echo hook-defined`,
	},
}

// locateFile returns a Locate that sources the first existing path.
func locateFile(candidates []string) func(string) (string, bool) {
	return func(string) (string, bool) {
		for _, raw := range candidates {
			path := os.ExpandEnv(raw)
			if path == "" || strings.Contains(path, "$") {
				continue
			}
			if _, err := os.Stat(path); err == nil {
				return ". " + shellQuote(path), true
			}
		}
		return "", false
	}
}

// locateEval returns a Locate that evals a tool's own init output — the
// form the tool's own documentation tells people to paste.
func locateEval(bin, command string) func(string) (string, bool) {
	return func(string) (string, bool) {
		if _, err := exec.LookPath(bin); err != nil {
			return "", false
		}
		return `eval "$(` + command + `)"`, true
	}
}

// locateVenv builds a real virtualenv to source, since the activate
// script is generated per environment and there is no system copy to
// point at. Built once per scratch directory.
func locateVenv(scratch string) (string, bool) {
	python, err := exec.LookPath("python3")
	if err != nil {
		return "", false
	}
	dir := filepath.Join(scratch, "venv")
	activate := filepath.Join(dir, "bin", "activate")
	if _, err := os.Stat(activate); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		if out, err := exec.CommandContext(ctx, python, "-m", "venv", dir).CombinedOutput(); err != nil {
			_ = out
			return "", false
		}
	}
	if _, err := os.Stat(activate); err != nil {
		return "", false
	}
	return ". " + shellQuote(activate), true
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// SourceResult is one case's verdict. Present=false means the tool is
// not installed here; the case was not run and says so.
type SourceResult struct {
	SourceCase
	Present            bool
	BashOut, GishOut   string
	BashCode, GishCode int
	Pass               bool
	Reason             string
}

// RunSource loads and probes one tool under both shells.
func RunSource(ctx context.Context, bashBin, gishBin, scratch string, c SourceCase) SourceResult {
	r := SourceResult{SourceCase: c}
	load, ok := c.Locate(scratch)
	if !ok {
		return r
	}
	r.Present = true
	script := load + "\n" + c.Probe
	r.BashOut, r.BashCode = runScript(ctx, bashBin, script)
	r.GishOut, r.GishCode = runScript(ctx, gishBin, script)
	switch {
	case r.BashOut == r.GishOut && r.BashCode == r.GishCode:
		r.Pass = true
	case r.BashOut != r.GishOut && r.BashCode != r.GishCode:
		r.Reason = "output and exit status differ"
	case r.BashOut != r.GishOut:
		r.Reason = "output differs"
	default:
		r.Reason = "exit status differs"
	}
	return r
}

// RunSourceAll runs every case whose tool is installed.
func RunSourceAll(ctx context.Context, bashBin, gishBin, scratch string) []SourceResult {
	out := make([]SourceResult, 0, len(SourceCorpus))
	for _, c := range SourceCorpus {
		out = append(out, RunSource(ctx, bashBin, gishBin, scratch, c))
	}
	return out
}
