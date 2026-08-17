// Package config resolves the zi-go home directory layout.
//
// Layout (default root ~/.zi-go, override with $ZI_GO_HOME):
//
//	plugins/<user---repo>/    cloned plugin repositories
//	snippets/<sanitized-url>/ downloaded single-file snippets
//	completions/              symlinked _* completion files (one fpath entry)
//	run/                      generated per-object load payloads (sourced by gish)
//
// The layout and $ZI_GO_HOME override are kept identical to standalone
// zi-go so existing installs carry over unchanged.
package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Home string
}

func Load() (*Config, error) {
	home := os.Getenv("ZI_GO_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".zi-go")
	}
	return &Config{Home: home}, nil
}

// Ensure creates one of the layout directories, on the way to writing
// something into it.
//
// Load deliberately creates nothing (#163). Resolving the layout happens
// at startup for every session, and a shell that has installed no
// plugins should not leave a four-directory tree in someone's home
// directory for the privilege of having been started once. Config
// hygiene is a documented churn cause on its own, and "it will be needed
// eventually" is not a reason to write to $HOME today.
func Ensure(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func (c *Config) PluginsDir() string     { return filepath.Join(c.Home, "plugins") }
func (c *Config) SnippetsDir() string    { return filepath.Join(c.Home, "snippets") }
func (c *Config) CompletionsDir() string { return filepath.Join(c.Home, "completions") }
func (c *Config) RunDir() string         { return filepath.Join(c.Home, "run") }
