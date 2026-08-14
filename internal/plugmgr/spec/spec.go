// Package spec parses plugin and snippet identifiers.
//
// Plugin forms accepted (mirroring Zi's .zi-any-to-user-plugin):
//
//	user/repo                → https://github.com/user/repo
//	https://host/user/repo   → cloned verbatim
//	repo (no slash)          → treated as user "", looked up locally only
//
// Snippet forms expand Zi's service aliases (OMZ::, OMZP::, OMZL::, OMZT::,
// PZT::, PZTM::) to raw.githubusercontent.com URLs.
package spec

import (
	"fmt"
	"strings"
)

type Kind int

const (
	Plugin Kind = iota
	Snippet
)

type Spec struct {
	Kind Kind
	// User/Repo are set for shorthand and recognized git-forge URLs.
	User string
	Repo string
	// URL is the resolved clone/download URL.
	URL string
	// ID is the directory-safe identifier (Zi's user---repo convention),
	// overridable with the id-as ice.
	ID string
	// Raw is the spec exactly as the user typed it.
	Raw string
}

// ForgeBases maps the `from` ice to a clone-URL base.
var ForgeBases = map[string]string{
	"gh":   "https://github.com",
	"gl":   "https://gitlab.com",
	"bb":   "https://bitbucket.org",
	"":     "https://github.com",
	"gh-r": "https://github.com", // installed from release assets; URL is informational
}

// ParsePlugin resolves a plugin spec. from is the value of the `from` ice
// ("" defaults to GitHub); idAs is the value of the `id-as` ice.
func ParsePlugin(raw, from, idAs string) (*Spec, error) {
	s := &Spec{Kind: Plugin, Raw: raw}
	switch {
	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"):
		s.URL = raw
		trimmed := strings.TrimSuffix(raw, ".git")
		parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(trimmed, "https://"), "http://"), "/")
		if len(parts) >= 3 {
			s.User, s.Repo = parts[len(parts)-2], parts[len(parts)-1]
		} else {
			s.Repo = parts[len(parts)-1]
		}
	case strings.Contains(raw, "/"):
		user, repo, _ := strings.Cut(raw, "/")
		if user == "" || repo == "" || strings.Contains(repo, "/") {
			return nil, fmt.Errorf("plugin spec %q: want user/repo, a URL, or a bare name", raw)
		}
		base, ok := ForgeBases[from]
		if !ok {
			return nil, fmt.Errorf("unknown from ice %q (want gh, gl, bb, gh-r, or a URL)", from)
		}
		s.User, s.Repo = user, repo
		s.URL = base + "/" + user + "/" + repo
	default:
		// Bare name: only resolvable if already installed under any user.
		s.Repo = raw
	}
	s.ID = idAs
	if s.ID == "" {
		if s.User != "" {
			s.ID = s.User + "---" + s.Repo
		} else {
			s.ID = Sanitize(s.Repo)
		}
	} else {
		s.ID = Sanitize(s.ID)
	}
	return s, nil
}

var snippetAliases = []struct{ prefix, base string }{
	{"OMZP::", "https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/plugins/"},
	{"OMZL::", "https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/lib/"},
	{"OMZT::", "https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/themes/"},
	{"OMZ::", "https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/"},
	{"PZTM::", "https://raw.githubusercontent.com/sorin-ionescu/prezto/master/modules/"},
	{"PZT::", "https://raw.githubusercontent.com/sorin-ionescu/prezto/master/"},
}

// ParseSnippet resolves a snippet spec to a download URL. OMZ plugin aliases
// (OMZP::git) expand to the plugin's main file, matching how Zi fetches
// single-file snippets over HTTP.
func ParseSnippet(raw, idAs string) (*Spec, error) {
	s := &Spec{Kind: Snippet, Raw: raw}
	url := raw
	for _, a := range snippetAliases {
		if rest, ok := strings.CutPrefix(raw, a.prefix); ok {
			url = a.base + rest
			// OMZP::name → the plugin's entry file inside its directory.
			if a.prefix == "OMZP::" && !strings.Contains(rest, "/") {
				url = a.base + rest + "/" + rest + ".plugin.zsh"
			}
			break
		}
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("snippet spec %q: want a URL or an OMZ::/OMZP::/OMZL::/OMZT::/PZT::/PZTM:: alias", raw)
	}
	s.URL = url
	if idAs != "" {
		s.ID = Sanitize(idAs)
	} else {
		s.ID = Sanitize(raw)
	}
	return s, nil
}

// Sanitize converts an identifier to a directory-safe name using Zi's
// convention of replacing path separators with "---".
func Sanitize(id string) string {
	id = strings.TrimPrefix(id, "https://")
	id = strings.TrimPrefix(id, "http://")
	id = strings.ReplaceAll(id, "/", "---")
	id = strings.ReplaceAll(id, ":", "-")
	return id
}
