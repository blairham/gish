package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Local AWS state, read-only: ~/.aws/config and ~/.aws/credentials for
// profile/region/SSO wiring, ~/.aws/sso/cache for token expiry. The
// plugin never calls AWS on the prompt path and never reads secret
// values — profile names, regions, and expiry timestamps only.

// Profile is one [profile x] section's prompt-relevant keys.
type Profile struct {
	Region     string
	SSOSession string
	SSOStart   string
}

// Config is the parsed local state.
type Config struct {
	Profiles map[string]Profile
	// SSOStartBySession maps [sso-session x] names to their start URLs.
	SSOStartBySession map[string]string
}

// loader caches by file mtimes so a prompt render costs stats, not
// parses.
type loader struct {
	home string

	mu     sync.Mutex
	mtimes [2]time.Time
	cached *Config
}

func newLoader(home string) *loader { return &loader{home: home} }

func (l *loader) configPath() string      { return filepath.Join(l.home, ".aws", "config") }
func (l *loader) credentialsPath() string { return filepath.Join(l.home, ".aws", "credentials") }

// Load parses (or serves the cached) local config.
func (l *loader) Load() *Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	var mtimes [2]time.Time
	for i, path := range []string{l.configPath(), l.credentialsPath()} {
		if fi, err := os.Stat(path); err == nil {
			mtimes[i] = fi.ModTime()
		}
	}
	if l.cached != nil && mtimes == l.mtimes {
		return l.cached
	}
	cfg := &Config{Profiles: map[string]Profile{}, SSOStartBySession: map[string]string{}}
	parseConfigFile(cfg, l.configPath())
	parseCredentialNames(cfg, l.credentialsPath())
	l.cached, l.mtimes = cfg, mtimes
	return cfg
}

// parseConfigFile reads the ini-shaped ~/.aws/config: [default],
// [profile x], and [sso-session x] sections with the keys we care for.
func parseConfigFile(cfg *Config, path string) {
	data, err := os.ReadFile(path) //nolint:gosec // the user's own config
	if err != nil {
		return
	}
	section, kind := "", ""
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(line[1 : len(line)-1])
			switch {
			case name == "default":
				section, kind = "default", "profile"
			case strings.HasPrefix(name, "profile "):
				section, kind = strings.TrimSpace(strings.TrimPrefix(name, "profile ")), "profile"
			case strings.HasPrefix(name, "sso-session "):
				section, kind = strings.TrimSpace(strings.TrimPrefix(name, "sso-session ")), "sso"
			default:
				section, kind = "", ""
			}
			if kind == "profile" {
				if _, ok := cfg.Profiles[section]; !ok {
					cfg.Profiles[section] = Profile{}
				}
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch kind {
		case "profile":
			p := cfg.Profiles[section]
			switch key {
			case "region":
				p.Region = value
			case "sso_session":
				p.SSOSession = value
			case "sso_start_url":
				p.SSOStart = value
			}
			cfg.Profiles[section] = p
		case "sso":
			if key == "sso_start_url" {
				cfg.SSOStartBySession[section] = value
			}
		}
	}
}

// parseCredentialNames adds profile names that exist only in the
// credentials file. Section names only — values are never read.
func parseCredentialNames(cfg *Config, path string) {
	data, err := os.ReadFile(path) //nolint:gosec // names only, values ignored
	if err != nil {
		return
	}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := cfg.Profiles[name]; !ok {
				cfg.Profiles[name] = Profile{}
			}
		}
	}
}

// ProfileNames returns the known profiles, sorted.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// startURL resolves the SSO start URL a profile authenticates against.
func (c *Config) startURL(profile string) string {
	p, ok := c.Profiles[profile]
	if !ok {
		return ""
	}
	if p.SSOStart != "" {
		return p.SSOStart
	}
	return c.SSOStartBySession[p.SSOSession]
}

// ssoExpiry scans ~/.aws/sso/cache for the token matching startURL and
// returns its expiry. Zero time = no cached token.
func ssoExpiry(home, startURL string) time.Time {
	if startURL == "" {
		return time.Time{}
	}
	entries, err := os.ReadDir(filepath.Join(home, ".aws", "sso", "cache"))
	if err != nil {
		return time.Time{}
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(home, ".aws", "sso", "cache", e.Name())) //nolint:gosec // token *metadata*
		if err != nil {
			continue
		}
		var tok struct {
			StartURL  string `json:"startUrl"`
			ExpiresAt string `json:"expiresAt"`
		}
		if json.Unmarshal(data, &tok) != nil || tok.StartURL != startURL {
			continue
		}
		if t, err := time.Parse(time.RFC3339, tok.ExpiresAt); err == nil {
			return t
		}
	}
	return time.Time{}
}

// awsRegions is the completion set for --region: the stable public
// region list. Static on purpose — completion must not call AWS.
var awsRegions = []string{
	"us-east-1", "us-east-2", "us-west-1", "us-west-2",
	"af-south-1", "ap-east-1", "ap-south-1", "ap-south-2",
	"ap-northeast-1", "ap-northeast-2", "ap-northeast-3",
	"ap-southeast-1", "ap-southeast-2", "ap-southeast-3", "ap-southeast-4",
	"ca-central-1", "ca-west-1",
	"eu-central-1", "eu-central-2", "eu-west-1", "eu-west-2", "eu-west-3",
	"eu-north-1", "eu-south-1", "eu-south-2",
	"il-central-1", "me-central-1", "me-south-1", "sa-east-1",
}
