// Package manifest is koi's declarative plugin configuration (#108):
// four concepts, no modifier language to memorize.
//
// The design goal, from docs/design.md: "declarative manifest, lazy
// loading by default, one obvious way to install/pin/update. No ice
// modifiers." Zi's ice syntax (from"gh-r" as"program" pick"bin/fzf"
// wait"1" lucid) is a small language whose vocabulary a user must learn
// before installing one plugin. This file replaces it with data:
//
//	[[plugin]]
//	source = "zsh-users/zsh-autosuggestions"
//
//	[[plugin]]
//	source = "junegunn/fzf"
//	kind   = "release"          # prebuilt binary, not source
//	pin    = "0.55.0"
//	lazy   = "command:fzf"      # load on first use of this command
//
// The zi engine still does the work underneath; this is the surface
// koi documents and the one `plugin add` writes.
package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Kind is how a source is obtained. A small closed set, deliberately
// not an extensible modifier vocabulary.
type Kind string

const (
	// KindPlugin clones a repository and sources its plugin file.
	KindPlugin Kind = "plugin"
	// KindRelease installs a prebuilt binary from GitHub releases.
	KindRelease Kind = "release"
	// KindSnippet sources a single remote file (OMZ:: aliases, URLs).
	KindSnippet Kind = "snippet"
)

// Plugin is one manifest entry. Zero values mean the obvious default:
// an eager, enabled, source plugin at its latest version.
type Plugin struct {
	// Source is "user/repo", an OMZ:: alias, or a URL.
	Source string `toml:"source"`
	// Kind defaults to plugin, or snippet when Source is a URL/alias.
	Kind Kind `toml:"kind,omitempty"`
	// Pin is a tag, branch, or release version; empty means latest.
	Pin string `toml:"pin,omitempty"`
	// Lazy defers loading until a trigger fires: "command:NAME" loads
	// on first use of that command. Empty loads at startup.
	Lazy string `toml:"lazy,omitempty"`
	// Enabled is true unless explicitly disabled — a way to keep an
	// entry while turning it off, instead of deleting and retyping it.
	Enabled *bool `toml:"enabled,omitempty"`
}

// On reports whether the entry should load.
func (p Plugin) On() bool { return p.Enabled == nil || *p.Enabled }

// EffectiveKind resolves the kind, inferring from the source shape when
// it was not stated.
func (p Plugin) EffectiveKind() Kind {
	if p.Kind != "" {
		return p.Kind
	}
	if strings.Contains(p.Source, "://") || strings.Contains(p.Source, "::") {
		return KindSnippet
	}
	return KindPlugin
}

// Name is the short identifier used in commands and messages.
func (p Plugin) Name() string {
	src := p.Source
	if i := strings.LastIndex(src, "::"); i >= 0 {
		src = src[i+2:]
	}
	if i := strings.LastIndex(src, "/"); i >= 0 && !strings.Contains(src, "://") {
		return src[i+1:]
	}
	if i := strings.LastIndex(src, "/"); i >= 0 {
		return src[i+1:]
	}
	return src
}

// Manifest is the parsed file.
type Manifest struct {
	Plugins []Plugin `toml:"plugin"`
}

// DefaultPath is $XDG_CONFIG_HOME/koi/plugins.toml.
func DefaultPath() (string, error) {
	confHome := os.Getenv("XDG_CONFIG_HOME")
	if confHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		confHome = filepath.Join(home, ".config")
	}
	return filepath.Join(confHome, "koi", "plugins.toml"), nil
}

// Load reads the manifest; a missing file is an empty manifest, not an
// error. A malformed file *is* an error — silently ignoring a broken
// plugin list would leave the user wondering why nothing loaded.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the user's own config
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &Manifest{}, nil
	case err != nil:
		return nil, err
	}
	var m Manifest
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i, p := range m.Plugins {
		if strings.TrimSpace(p.Source) == "" {
			return nil, fmt.Errorf("%s: plugin %d has no source", path, i+1)
		}
		if k := p.EffectiveKind(); k != KindPlugin && k != KindRelease && k != KindSnippet {
			return nil, fmt.Errorf("%s: plugin %q has unknown kind %q (plugin, release, snippet)",
				path, p.Source, k)
		}
	}
	return &m, nil
}

// Find returns the index of the entry matching name or source.
func (m *Manifest) Find(nameOrSource string) int {
	return slices.IndexFunc(m.Plugins, func(p Plugin) bool {
		return p.Source == nameOrSource || p.Name() == nameOrSource
	})
}

// Add appends or replaces an entry, returning whether it replaced.
func (m *Manifest) Add(p Plugin) bool {
	if i := m.Find(p.Source); i >= 0 {
		m.Plugins[i] = p
		return true
	}
	m.Plugins = append(m.Plugins, p)
	return false
}

// Remove drops an entry, reporting whether it existed.
func (m *Manifest) Remove(nameOrSource string) bool {
	i := m.Find(nameOrSource)
	if i < 0 {
		return false
	}
	m.Plugins = slices.Delete(m.Plugins, i, i+1)
	return true
}

// Save writes the manifest, creating the directory. Write-then-rename:
// a half-written plugin list must never be what the next shell reads.
func (m *Manifest) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# koi plugins — `plugin add|remove|pin|enable|disable` edit this file,\n")
	b.WriteString("# and hand edits are equally fine. Four knobs: source, kind, pin, lazy.\n")
	enc := toml.NewEncoder(&b)
	if err := enc.Encode(m); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil { //nolint:gosec // config, not a secret
		return err
	}
	return os.Rename(tmp, path)
}
