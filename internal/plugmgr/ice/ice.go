// Package ice models Zi's ice modifiers — per-invocation options that shape
// how a plugin or snippet is installed and loaded (zi's `zi ice wait"1" lucid`).
//
// The shim collects `zi ice ...` arguments in the shell and forwards them to
// the binary as repeated --ice key=value flags; flag-style ices (lucid, silent)
// arrive with an empty value.
package ice

import (
	"fmt"
	"sort"
	"strings"
)

// Known enumerates the ice modifiers this rewrite understands. Unknown ices
// are accepted (stored, persisted, reported) but have no effect, so a config
// written for zsh Zi degrades gracefully instead of erroring.
var Known = map[string]string{
	"wait":          "postpone loading until N seconds after the first prompt (turbo mode)",
	"lucid":         "suppress the under-prompt 'Loaded ...' message",
	"silent":        "mute stdout/stderr of hooks and loading",
	"as":            `"program" adds the plugin dir to PATH instead of sourcing; "completion" only installs completions; "null" installs without sourcing or PATH`,
	"from":          `clone source: "gh" (default), "gl", "bb", "gh-r" (GitHub release binaries), or a raw URL`,
	"ver":           "git branch/tag/commit to check out (or release tag for from'gh-r')",
	"pick":          "glob selecting the file to source (or the binary for as'program')",
	"src":           "additional file to source after the main one",
	"multisrc":      "space-separated list of additional files to source",
	"atclone":       "shell command run in the plugin dir right after cloning",
	"atpull":        "shell command run in the plugin dir after updating",
	"atinit":        "zsh snippet run in the user shell before sourcing",
	"atload":        "zsh snippet run in the user shell after sourcing",
	"make":          "run make in the plugin dir after clone/update; value passed as make args",
	"mv":            `rename after clone: "from -> to" (glob supported on the left)`,
	"cp":            `copy after clone: "from -> to" (glob supported on the left)`,
	"bpick":         "glob selecting which release asset to download with from'gh-r'",
	"id-as":         "install under this identifier instead of the derived one",
	"if":            "zsh condition; load only when it evaluates true",
	"has":           "load only when this command exists in PATH",
	"nocompletions": "skip completion discovery/linking",
	"blockf":        "block plugin-driven fpath modifications (approximated: no-op)",
	"light-mode":    "load without tracking (all loads are light in zi-go)",
	"depth":         "shallow-clone depth (default 0 = full clone)",
}

// Register adds an annex-provided ice to Known so the parser's prefix
// matching treats it like a built-in (dl'url -> file' parses as dl). Existing
// built-ins are never overridden.
func Register(name, description string) {
	if name == "" {
		return
	}
	if _, ok := Known[name]; ok {
		return
	}
	Known[name] = description
}

type Ices struct {
	m map[string]string
}

func New() *Ices { return &Ices{m: map[string]string{}} }

func FromMap(m map[string]string) *Ices {
	i := New()
	for k, v := range m {
		i.m[k] = v
	}
	return i
}

// ParseArgs parses repeated --ice arguments of the form "key=value" or bare
// "key" for flag ices. It also accepts Zi's native syntax key"value" /
// key'value' so configs can be pasted through unchanged.
func ParseArgs(args []string) (*Ices, error) {
	i := New()
	for _, raw := range args {
		key, val, err := parseOne(raw)
		if err != nil {
			return nil, err
		}
		i.m[key] = val
	}
	return i, nil
}

// parseOne accepts three shapes:
//
//	wait1, pickinit.zsh   — Zi's native syntax after the shell strips quotes
//	                        (wait"1" → wait1): longest known ice name prefix,
//	                        remainder is the value
//	wait=1, unknown=x     — explicit key=value
//	lucid, wait           — bare ice name, empty value
func parseOne(raw string) (key, val string, err error) {
	if raw == "" {
		return "", "", fmt.Errorf("empty ice")
	}
	if _, known := Known[raw]; known {
		return raw, "", nil
	}
	// Longest known prefix wins so id-as beats a hypothetical id, and the
	// remainder — quoted or not — becomes the value.
	best := ""
	for name := range Known {
		if strings.HasPrefix(raw, name) && len(name) > len(best) {
			best = name
		}
	}
	if best != "" {
		val := strings.TrimPrefix(raw, best)
		val = strings.TrimPrefix(val, "=")
		val = trimMatchedQuotes(val)
		return best, val, nil
	}
	if k, v, ok := strings.Cut(raw, "="); ok {
		return k, trimMatchedQuotes(v), nil
	}
	return raw, "", nil
}

func trimMatchedQuotes(s string) string {
	for _, q := range []string{`"`, `'`} {
		if len(s) >= 2 && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) {
			return strings.TrimPrefix(strings.TrimSuffix(s, q), q)
		}
	}
	return s
}

func (i *Ices) Get(key string) string  { return i.m[key] }
func (i *Ices) Has(key string) bool    { _, ok := i.m[key]; return ok }
func (i *Ices) Set(key, val string)    { i.m[key] = val }
func (i *Ices) Map() map[string]string { return i.m }
func (i *Ices) IsEmpty() bool          { return len(i.m) == 0 }

// Keys returns the ice names in stable order (for reports and golden tests).
func (i *Ices) Keys() []string {
	keys := make([]string, 0, len(i.m))
	for k := range i.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (i *Ices) String() string {
	parts := make([]string, 0, len(i.m))
	for _, k := range i.Keys() {
		if v := i.m[k]; v == "" {
			parts = append(parts, k)
		} else {
			parts = append(parts, fmt.Sprintf("%s%q", k, v))
		}
	}
	return strings.Join(parts, " ")
}
