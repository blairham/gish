package p10k

import (
	"slices"
	"sort"
	"strings"
)

// Settings that are stored faithfully and not acted on (#133).
//
// A setting has three possible states, and only two of them used to be
// visible: absent, set-and-honoured, and set-and-ignored. The third is
// the worst one, because the user has no way to tell it from the second
// — they configured something, `p10k show` reported it, and the prompt
// quietly did something else.
//
// This list is the third state, written down. It shrinks as things are
// implemented, which is the point: an entry here is a promise to either
// do it or say why.
var unimplementedSettings = map[string]string{
	"SHORTEN_STRATEGY=truncate_to_unique": "lists every parent's siblings on each prompt; gish shortens to first characters instead, with no I/O",
	"DIR_ANCHOR_FROM_MARKER":              "not implemented",
	"TRANSIENT_PROMPT=same-dir":           "gish trims before the command runs, so the comparison is inverted (see docs/p10k.md)",
	"INSTANT_PROMPT":                      instantPromptReason,
}

const instantPromptReason = "not needed: gish resolves a full p10k prompt in ~7ms, so there is nothing to cache ahead of it"

// UnhonouredSettings reports the settings this config sets that the
// engine does not act on, so `p10k show` can name them.
func (c *Config) UnhonouredSettings() []string {
	var out []string
	for key, why := range unimplementedSettings {
		name, value, hasValue := strings.Cut(key, "=")
		set := c.Has(name)
		if hasValue {
			set = set && strings.EqualFold(strings.TrimSpace(c.Str(name, "")), value)
		}
		if !set {
			continue
		}
		out = append(out, key+" — "+why)
	}
	// A segment that is configured but has no implementation is the same
	// class of gap, and `not yet` already reports those by name.
	sort.Strings(out)
	return slices.Clip(out)
}
