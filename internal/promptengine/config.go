package promptengine

import (
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Config is the parameter namespace the whole engine reads from.
//
// Upstream's configuration is ~565 POWERLEVEL9K_* shell variables. That
// is not 565 features — it is one generic mechanism (a segment asks for
// a key, the lookup walks a fallback chain) applied to every segment.
// Modeling it as a keyed store rather than a Go struct is what keeps
// presets as data: a preset is a set of assignments, and adding a
// segment costs no new config plumbing.
//
// Keys are stored *without* the POWERLEVEL9K_ prefix and always upper
// case, so DIR_FOREGROUND, LEFT_PROMPT_ELEMENTS, MODE. Values are either
// scalars or lists; a key is one or the other, never both.
type Config struct {
	scalars map[string]string
	lists   map[string][]string

	// Sources records where the settings came from, oldest layer first,
	// for `prompt show` and the doctor line.
	Sources []string

	// Unsupported records settings that were present but cannot be
	// honored natively (a CONTENT_EXPANSION calling a shell function,
	// say). Reported, never silently dropped.
	Unsupported []string
}

// NewConfig returns an empty namespace.
func NewConfig() *Config {
	return &Config{scalars: map[string]string{}, lists: map[string][]string{}}
}

// normKey accepts any spelling a caller might have — with or without the
// POWERLEVEL9K_/P9K_ prefix, any case — and returns the canonical key.
func normKey(key string) string {
	k := strings.ToUpper(strings.TrimSpace(key))
	k = strings.TrimPrefix(k, "POWERLEVEL9K_")
	k = strings.TrimPrefix(k, "POWERLEVEL10K_")
	k = strings.TrimPrefix(k, "P9K_")
	return k
}

// Set stores a scalar, replacing any list previously held under the key.
func (c *Config) Set(key, value string) {
	k := normKey(key)
	delete(c.lists, k)
	c.scalars[k] = value
}

// SetList stores a list, replacing any scalar previously held.
func (c *Config) SetList(key string, values []string) {
	k := normKey(key)
	delete(c.scalars, k)
	c.lists[k] = slices.Clone(values)
}

// Has reports whether the key is set at all, which is distinct from
// being set to the empty string: upstream treats an explicitly empty
// value as a real answer (an empty prefix, a suppressed icon), so
// callers that need that distinction must ask.
func (c *Config) Has(key string) bool {
	k := normKey(key)
	_, scalar := c.scalars[k]
	_, list := c.lists[k]
	return scalar || list
}

// Str returns a scalar setting, or def when unset.
func (c *Config) Str(key, def string) string {
	if v, ok := c.scalars[normKey(key)]; ok {
		return v
	}
	return def
}

// List returns a list setting. A scalar set under the key reads as a
// one-element list, matching how shell arrays and strings interchange
// upstream; unset returns nil.
func (c *Config) List(key string) []string {
	k := normKey(key)
	if v, ok := c.lists[k]; ok {
		return slices.Clone(v)
	}
	if v, ok := c.scalars[k]; ok && v != "" {
		return []string{v}
	}
	return nil
}

// Bool reads a truth setting. Upstream writes these as the strings true
// and false; 1/0, yes/no and on/off are accepted too, since the native
// config surface and the gish `config` command speak those.
func (c *Config) Bool(key string, def bool) bool {
	v, ok := c.scalars[normKey(key)]
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off", "":
		return false
	}
	return def
}

// Int reads a numeric setting, falling back to def when unset or
// unparsable — a malformed value degrades, it never breaks the prompt.
func (c *Config) Int(key string, def int) int {
	v, ok := c.scalars[normKey(key)]
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

// Param is the segment-scoped lookup, and the reason this is a keyed
// store. Upstream resolves every per-segment setting through the same
// three steps, most specific first:
//
//	POWERLEVEL9K_<SEGMENT>_<STATE>_<KEY>   dir_NOT_WRITABLE_FOREGROUND
//	POWERLEVEL9K_<SEGMENT>_<KEY>           dir_FOREGROUND
//	POWERLEVEL9K_<KEY>                     FOREGROUND
//
// then the caller's default. state may be empty, which drops the first
// step. That chain is why a preset can set one FOREGROUND and have every
// segment inherit it, while any segment or any single state overrides.
func (c *Config) Param(segment, state, key, def string) string {
	for _, name := range paramChain(segment, state, key) {
		if v, ok := c.scalars[name]; ok {
			return v
		}
	}
	return def
}

// ParamSet reports whether any step of the chain is set, so a caller can
// tell "explicitly empty" from "absent" the way Has does for plain keys.
func (c *Config) ParamSet(segment, state, key string) bool {
	for _, name := range paramChain(segment, state, key) {
		if _, ok := c.scalars[name]; ok {
			return true
		}
	}
	return false
}

// ParamBool is Param with the truth-value parsing of Bool.
func (c *Config) ParamBool(segment, state, key string, def bool) bool {
	for _, name := range paramChain(segment, state, key) {
		if _, ok := c.scalars[name]; ok {
			return c.Bool(name, def)
		}
	}
	return def
}

// ParamInt is Param with the numeric parsing of Int.
func (c *Config) ParamInt(segment, state, key string, def int) int {
	for _, name := range paramChain(segment, state, key) {
		if _, ok := c.scalars[name]; ok {
			return c.Int(name, def)
		}
	}
	return def
}

// paramChain builds the lookup order for Param and friends.
func paramChain(segment, state, key string) []string {
	seg := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(segment), "-", "_"))
	st := strings.ToUpper(strings.TrimSpace(state))
	k := normKey(key)
	switch {
	case seg == "":
		return []string{k}
	case st == "":
		return []string{seg + "_" + k, k}
	default:
		return []string{seg + "_" + st + "_" + k, seg + "_" + k, k}
	}
}

// Merge layers other on top of c: every key other sets wins, keys it
// does not mention keep c's value. Layering (defaults, then the user's
// file, then session overrides) is the whole configuration story, so
// this is deliberately a shallow per-key overwrite and not a deep merge
// — a list set by a later layer replaces the earlier list outright,
// which is what "my elements are these" has to mean.
func (c *Config) Merge(other *Config) {
	if other == nil {
		return
	}
	for k, v := range other.scalars {
		delete(c.lists, k)
		c.scalars[k] = v
	}
	for k, v := range other.lists {
		delete(c.scalars, k)
		c.lists[k] = slices.Clone(v)
	}
	c.Sources = append(c.Sources, other.Sources...)
	c.Unsupported = append(c.Unsupported, other.Unsupported...)
}

// Clone returns a deep copy, so a layer can be reused as a base without
// a later merge writing through to it.
func (c *Config) Clone() *Config {
	out := &Config{
		scalars:     maps.Clone(c.scalars),
		lists:       make(map[string][]string, len(c.lists)),
		Sources:     slices.Clone(c.Sources),
		Unsupported: slices.Clone(c.Unsupported),
	}
	for k, v := range c.lists {
		out.lists[k] = slices.Clone(v)
	}
	return out
}

// Keys returns every set key, sorted — for `prompt show` and tests.
func (c *Config) Keys() []string {
	keys := make([]string, 0, len(c.scalars)+len(c.lists))
	for k := range c.scalars {
		keys = append(keys, k)
	}
	for k := range c.lists {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
