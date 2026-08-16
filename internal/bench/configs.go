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
			// The marker cannot be GISH_PROMPT here: a manual prompt
			// outranks the theme, so setting it would measure a string
			// literal and call it a theme. Instead the theme renders in
			// full — directory, git, every segment — and its prompt
			// character *is* the marker, so the clock stops only once
			// the whole prompt has been computed.
			Env: append(slicesClone(gishEnv),
				"GISH_THEME=p10k",
				"POWERLEVEL9K_PROMPT_CHAR_OK_CONTENT_EXPANSION="+marker,
				"POWERLEVEL9K_PROMPT_CHAR_ERROR_CONTENT_EXPANSION="+marker),
			Note: "the full native powerlevel10k engine, every segment resolved",
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

// PowerlevelConfig is the head-to-head: real zsh running real
// powerlevel10k, against gish running its native port of it.
//
// This is the only comparison that answers "is the port actually
// faster", and it is deliberately generous to upstream — instant prompt
// is left on, since that is how people run it, and the marker is
// appended as the last precmd so the clock stops when the *real* prompt
// is ready rather than when the cached one is painted. Without that, a
// theme with an instant prompt appears to start in no time at all, which
// is exactly the illusion instant prompt exists to create.
//
// Returns false when powerlevel10k is not installed, and the row is
// reported as missing rather than estimated.
func PowerlevelConfig() (Config, bool) {
	zsh := look("zsh")
	if zsh == "" {
		return Config{}, false
	}
	themeFile, ok := findPowerlevel10k()
	if !ok {
		return Config{}, false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, false
	}

	// Use the measuring user's own p10k configuration when they have
	// one: a stock config measures a prompt nobody runs.
	rc := "source " + themeFile + "\n"
	note := "powerlevel10k with its shipped defaults"
	if p10krc := filepath.Join(home, ".p10k.zsh"); fileExists(p10krc) {
		rc += "source " + p10krc + "\n"
		note = "powerlevel10k with the measuring user's own .p10k.zsh"
	}
	rc += "gish_bench_marker() { print -n '" + marker + " ' }\n" +
		"precmd_functions+=(gish_bench_marker)\n"

	return Config{
		Label: "zsh + powerlevel10k", Bin: zsh,
		Args: []string{"-i"},
		Env: []string{
			"HOME=" + home, "ZDOTDIR={{home}}",
			"TERM=xterm-256color", "PATH=" + os.Getenv("PATH"),
		},
		RC:   rc,
		Note: note + " — the thing gish's p10k theme is a port of",
	}, true
}

// findPowerlevel10k looks where the common installers put it.
func findPowerlevel10k() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	candidates := []string{
		".zi/plugins/romkatv---powerlevel10k",
		".oh-my-zsh/custom/themes/powerlevel10k",
		".zinit/plugins/romkatv---powerlevel10k",
		"powerlevel10k",
		".config/zsh/powerlevel10k",
	}
	for _, dir := range candidates {
		path := filepath.Join(home, dir, "powerlevel10k.zsh-theme")
		if fileExists(path) {
			return path, true
		}
	}
	for _, prefix := range []string{"/opt/homebrew/share", "/usr/local/share", "/usr/share"} {
		path := filepath.Join(prefix, "powerlevel10k", "powerlevel10k.zsh-theme")
		if fileExists(path) {
			return path, true
		}
	}
	return "", false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
