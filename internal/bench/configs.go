package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The measured matrix. Every row sets its prompt to the shared marker
// so "first prompt" is the same event across shells, and every row
// carries a note saying what it includes — a startup table without
// that column is a magic trick.

// StartupConfigs builds the comparison matrix for the shells present
// on this machine. gishBin is the binary under test.
func StartupConfigs(gishBin string) []Config {
	gishEnv := []string{
		"HOME={{home}}",
		"XDG_CONFIG_HOME={{home}}/config",
		"XDG_DATA_HOME={{home}}/data",
		"XDG_STATE_HOME={{home}}/state",
		"TERM=xterm-256color",
		"PATH=" + os.Getenv("PATH"),
	}
	configs := []Config{
		{
			Label: "gish (naked default)", Bin: gishBin,
			Env:  append(slicesClone(gishEnv), "GISH_PROMPT="+marker+" "),
			Note: "out-of-box: no rc, no plugins, stock prompt",
		},
		{
			Label: "gish (p10k theme)", Bin: gishBin,
			Env: append(slicesClone(gishEnv),
				"GISH_THEME=p10k", "GISH_PROMPT_MARKER=1", "GISH_PROMPT="+marker+" "),
			Note: "native two-line theme engine loaded",
		},
		{
			Label: "gish (lint + highlight + suggestions)", Bin: gishBin,
			Env: append(slicesClone(gishEnv),
				"GISH_LINT=on", "GISH_PROMPT="+marker+" "),
			Note: "every interactive feature on",
		},
	}

	if bash := look("bash"); bash != "" {
		configs = append(configs, Config{
			Label: "bash (no rc)", Bin: bash,
			Args: []string{"--rcfile", "{{rc}}", "-i"},
			Env:  []string{"HOME={{home}}", "TERM=xterm-256color", "PATH=" + os.Getenv("PATH")},
			RC:   "PS1='" + marker + " '\n",
			Note: "empty rc: the floor for a bash-family shell",
		})
	}
	if zsh := look("zsh"); zsh != "" {
		configs = append(configs, Config{
			Label: "zsh (no rc)", Bin: zsh,
			Args: []string{"-i"},
			Env: []string{
				"HOME={{home}}", "ZDOTDIR={{home}}",
				"TERM=xterm-256color", "PATH=" + os.Getenv("PATH"),
			},
			RC:   "PROMPT='" + marker + " '\n",
			Note: "empty rc: zsh's own floor",
		})
	}
	if dash := look("dash"); dash != "" {
		configs = append(configs, Config{
			Label: "dash (POSIX baseline)", Bin: dash,
			Args: []string{"-i"},
			Env: []string{
				"HOME={{home}}", "TERM=xterm-256color",
				"PATH=" + os.Getenv("PATH"), "PS1=" + marker + " ",
			},
			Note: "not a competitor — the floor a process-spawn can reach",
		})
	}
	if fish := look("fish"); fish != "" {
		configs = append(configs, Config{
			Label: "fish (no config)", Bin: fish,
			Args: []string{"-i", "-C", "function fish_prompt; echo -n '" + marker + " '; end"},
			Env: []string{
				"HOME={{home}}", "XDG_CONFIG_HOME={{home}}/config",
				"TERM=xterm-256color", "PATH=" + os.Getenv("PATH"),
			},
			Note: "empty config",
		})
	} else {
		configs = append(configs, Config{Label: "fish", Note: "not installed on the measuring machine"})
	}
	if nu := look("nu"); nu != "" {
		configs = append(configs, Config{
			Label: "nushell (no config)", Bin: nu,
			Args: []string{"-i", "--no-config-file"},
			Env: []string{
				"HOME={{home}}", "TERM=xterm-256color", "PATH=" + os.Getenv("PATH"),
				"PROMPT_COMMAND=" + marker,
			},
			Note: "empty config",
		})
	} else {
		configs = append(configs, Config{Label: "nushell", Note: "not installed on the measuring machine"})
	}
	return configs
}

// RealZshConfig measures the measuring user's actual zsh setup when
// one exists — the honest "loaded config" data point that an empty rc
// can never show. It is labeled as machine-specific because it is.
func RealZshConfig() (Config, bool) {
	zsh := look("zsh")
	home, err := os.UserHomeDir()
	if zsh == "" || err != nil {
		return Config{}, false
	}
	rc := filepath.Join(home, ".zshrc")
	data, err := os.ReadFile(rc) //nolint:gosec // the measuring user's own rc
	if err != nil {
		return Config{}, false
	}
	lines := strings.Count(string(data), "\n")
	return Config{
		Label: "zsh (this machine's real config)", Bin: zsh,
		Args: []string{"-i"},
		Env: []string{
			"HOME=" + home, "TERM=xterm-256color", "PATH=" + os.Getenv("PATH"),
			// The real rc runs; the marker is appended by a PROMPT
			// override in the temp ZDOTDIR that sources it.
			"ZDOTDIR={{home}}",
		},
		// A static PROMPT is not enough: themes like p10k re-render the
		// prompt from precmd hooks, so the marker is appended as the
		// last precmd instead — it prints once the real prompt is ready.
		RC: "source " + rc + "\n" +
			"gish_bench_marker() { print -n '" + marker + " ' }\n" +
			"precmd_functions+=(gish_bench_marker)\n",
		Note: "the measuring user's own " + itoa(lines) + "-line .zshrc (plugin manager, theme, tool hooks)",
	}, true
}

// KeystrokeScenarios are the typing situations worth publishing.
func KeystrokeScenarios() []KeystrokeScenario {
	return []KeystrokeScenario{
		{
			Scenario: "plain insert", Key: 'x',
			Env:  []string{"NO_COLOR=1", "GISH_LINT=off"},
			Note: "no highlighting, no suggestions: the editor floor",
		},
		{
			Scenario: "insert with highlighting", Key: 'x',
			Prefix: "ec",
			Note:   "parser-driven syntax highlighting active",
		},
		{
			Scenario: "insert with highlight + suggestions", Key: 'o',
			Prefix: "ech",
			Env:    []string{"GISH_LINT=on"},
			Note:   "highlighting, history ghost text, and footgun lint all on",
		},
		{
			Scenario: "insert mid-command with lint", Key: 'p',
			Prefix: "rm -rf $tm",
			Env:    []string{"GISH_LINT=on"},
			Note:   "the lint path with a real finding to report",
		},
		{
			Scenario: "Tab completion (core providers)", Key: '\t',
			Prefix: "ech",
			Note:   "command-name completion, no plugins installed",
		},
	}
}

func look(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }
