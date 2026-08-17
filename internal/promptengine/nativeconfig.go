package promptengine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The native configuration file: the only thing read at render time.
//
// The format is the parameter namespace written down, one setting per
// line, because the namespace is already the model and a second
// vocabulary on top of it would only be something to keep in sync:
//
//	# gish p10k configuration
//	preset = lean
//	DIR_FOREGROUND = 31
//	LEFT_PROMPT_ELEMENTS = dir vcs newline prompt_char
//
// Keys are the POWERLEVEL9K_* names with the prefix dropped (the prefix
// is accepted on input, so a line pasted from a .p10k.zsh works). It is
// deliberately not TOML or YAML: there is no nesting to express, and a
// prompt configuration should be greppable and diffable.

// ConfigFileName is the file's name inside the gish config directory.
const ConfigFileName = "p10k.conf"

// ConfigPath returns where the native configuration lives, honoring
// XDG_CONFIG_HOME.
func ConfigPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "gish", ConfigFileName), nil
}

// LoadNativeConfig reads the native configuration, returning nil when
// there is none. A malformed line is skipped and recorded rather than
// fatal: a typo in a prompt config should cost you that one setting, not
// your shell.
//
// The result is cached on the file's mtime, so a prompt costs one stat
// and an edit takes effect on the next prompt without a restart — the
// hot reload upstream has, for free.
func LoadNativeConfig() *Config {
	path, err := ConfigPath()
	if err != nil {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if cached, ok := fileCache.Load(path); ok {
		if entry := cached.(fileEntry); entry.stamp.Equal(fi.ModTime()) {
			return entry.cfg
		}
	}
	cfg := LoadConfigFile(path)
	fileCache.Store(path, fileEntry{stamp: fi.ModTime(), cfg: cfg})
	return cfg
}

// fileCache holds parsed configuration files by path.
var fileCache sync.Map // path -> fileEntry

type fileEntry struct {
	stamp time.Time
	cfg   *Config
}

// LoadConfigFile reads a native configuration from an explicit path.
func LoadConfigFile(path string) *Config {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	cfg := NewConfig()
	cfg.Sources = append(cfg.Sources, path)

	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, found := strings.Cut(text, "=")
		if !found {
			cfg.Unsupported = append(cfg.Unsupported,
				fmt.Sprintf("%s:%d: not a setting: %q", filepath.Base(path), line, text))
			continue
		}
		key = strings.TrimSpace(key)
		value = unquoteValue(value)
		if isListKey(key) {
			cfg.SetList(key, strings.Fields(value))
			continue
		}
		cfg.Set(key, value)
	}
	return cfg
}

// unquoteValue reads the right-hand side of a setting.
//
// Whitespace is significant here, which is easy to miss: the lean preset
// and every configuration derived from it separate segments by setting
// LEFT_SUBSEGMENT_SEPARATOR to a single space. Trimming the value on the
// way in turns that space into the empty string, and the prompt renders
// with its segments run together — `~/dev/gishmain` instead of
// `~/dev/gish main`. So surrounding space is stripped only for unquoted
// values; a quoted value keeps exactly what is between the quotes.
func unquoteValue(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 {
		if q := v[0]; (q == '"' || q == '\'') && v[len(v)-1] == q {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// quoteValue is unquoteValue's inverse: it quotes only when writing the
// value bare would not read back identically.
func quoteValue(v string) string {
	if v == "" || v == strings.TrimSpace(v) && !needsQuotes(v) {
		return v
	}
	return `"` + v + `"`
}

// needsQuotes reports whether a value would be misread unquoted — one
// that already looks quoted, or that a comment-stripping reader would
// cut short.
func needsQuotes(v string) bool {
	if len(v) >= 2 {
		if q := v[0]; (q == '"' || q == '\'') && v[len(v)-1] == q {
			return true // would be unquoted on the way back in
		}
	}
	return false
}

// isListKey names the settings that hold several values. Keeping this an
// explicit list, rather than inferring from whitespace, means a value
// that happens to contain a space stays one value.
func isListKey(key string) bool {
	k := normKey(key)
	return strings.HasSuffix(k, "_PROMPT_ELEMENTS")
}

// SaveNativeConfig writes settings to the native configuration file,
// creating the directory when needed. The write is atomic: a prompt
// configuration half-written by an interrupted wizard would be worse
// than one not written at all.
func SaveNativeConfig(cfg *Config) (string, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("# gish prompt configuration\n")
	b.WriteString("# Written by `prompt configure`. Edit freely: one setting per line.\n\n")
	for _, key := range cfg.Keys() {
		if values := cfg.lists[key]; values != nil {
			fmt.Fprintf(&b, "%s = %s\n", key, strings.Join(values, " "))
			continue
		}
		fmt.Fprintf(&b, "%s = %s\n", key, quoteValue(cfg.scalars[key]))
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}
