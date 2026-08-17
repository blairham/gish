package p10k

import (
	"os"
	"path/filepath"
	"strings"
)

// The dir settings that imported cleanly and were not acted on (#133).
//
// `p10k import` took them faithfully, `p10k show` reported them, and the
// prompt ignored them — which is the worst of the three states, because
// the user has no way to tell. These are the ones that change what a
// path *means* to read, which is the segment's whole job.

// DirClass is one POWERLEVEL9K_DIR_CLASSES entry: a pattern, the class
// name whose parameters then apply, and an optional icon.
//
// Upstream's shape is a flat list of triples — (pattern, class, icon) —
// and the first pattern that matches wins. The class name is what turns
// into a parameter *state*, so `POWERLEVEL9K_DIR_WORK_FOREGROUND` is
// how a class gets its color: it costs no new mechanism, because the
// three-step parameter chain already resolves SEGMENT_STATE_KEY.
type DirClass struct {
	Pattern string
	Class   string
	Icon    string
}

// dirClasses reads the configured classes.
func dirClasses(cfg *Config) []DirClass {
	raw := cfg.List("DIR_CLASSES")
	classes := make([]DirClass, 0, len(raw)/3)
	for i := 0; i+1 < len(raw); i += 3 {
		c := DirClass{Pattern: raw[i], Class: raw[i+1]}
		if i+2 < len(raw) {
			c.Icon = raw[i+2]
		}
		classes = append(classes, c)
	}
	return classes
}

// classifyDir returns the class name and icon for a directory, or empty
// strings when no class matches.
//
// Matching is glob, as upstream's is, against both the real path and its
// tilde form: people write `~/work/*` in their config and the pattern
// has to match the thing they wrote, not only the absolute path it
// expands to.
func classifyDir(cfg *Config, dir, home string) (class, icon string) {
	classes := dirClasses(cfg)
	if len(classes) == 0 {
		return "", ""
	}
	candidates := []string{dir}
	if t := tildify(dir, home); t != dir {
		candidates = append(candidates, t)
	}
	for _, c := range classes {
		for _, path := range candidates {
			if globMatchDir(c.Pattern, path) {
				return c.Class, c.Icon
			}
		}
	}
	return "", ""
}

// globMatchDir matches a path against a pattern, treating a trailing
// `*` as "and everything below" — which is what `~/work/*` means to
// someone writing it, even though filepath.Match's `*` stops at a
// separator.
func globMatchDir(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	if ok, err := filepath.Match(pattern, path); err == nil && ok {
		return true
	}
	if prefix, found := strings.CutSuffix(pattern, "*"); found {
		prefix = strings.TrimSuffix(prefix, string(os.PathSeparator))
		if path == prefix || strings.HasPrefix(path, prefix+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// hyperlinkOpen and hyperlinkClose wrap a path in an OSC 8 link.
//
// Safe because the renderer discounts escape sequences from every width
// calculation (#157): a hyperlinked path occupies exactly the columns
// its text does, which is the property that keeps the layout arithmetic
// working — and the reason this setting was already half-ready.
const hyperlinkClose = "\x1b]8;;\x1b\\"

func hyperlinkOpen(dir string) string {
	if dir == "" {
		return ""
	}
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	return "\x1b]8;;file://" + host + encodeURLPath(dir) + "\x1b\\"
}

func encodeURLPath(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		switch {
		case r == '/' || r == '-' || r == '_' || r == '.' || r == '~',
			r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			for _, c := range []byte(string(r)) {
				const hex = "0123456789ABCDEF"
				b.WriteByte('%')
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			}
		}
	}
	return b.String()
}

// markerDirs finds the components that must stay whole:
// SHORTEN_FOLDER_MARKER names files (`.git`, `go.mod`) whose presence
// makes a directory worth reading in full, and DIR_TRUNCATE_BEFORE_MARKER
// says to drop everything above the last such directory entirely.
//
// This is the one dir setting that needs the filesystem, which is why it
// was skipped. It costs one stat per component per marker, and only when
// the setting is set — the memo below keeps a repeated prompt in the
// same directory from paying it twice.
func markerIndices(cfg *Config, ctx *Context, absPath string) (indices []int) {
	markers := strings.Fields(cfg.Str("SHORTEN_FOLDER_MARKER", ""))
	if len(markers) == 0 {
		return nil
	}
	sep := string(os.PathSeparator)
	parts := strings.Split(absPath, sep)
	prefix := ""
	for i, part := range parts {
		switch {
		case i == 0 && part == "":
			prefix = sep
		case prefix == sep:
			prefix = sep + part
		case prefix == "":
			prefix = part
		default:
			prefix += sep + part
		}
		for _, m := range markers {
			if ctx.exists(filepath.Join(prefix, m)) {
				indices = append(indices, i)
				break
			}
		}
	}
	return indices
}

// commandColumns is DIR_MIN_COMMAND_COLUMNS and _PCT: leave the command
// line room. Upstream's rule is that the directory gives way rather
// than the thing being typed, which is the right priority — a path is
// context, and the command is the work.
func commandColumns(cfg *Config, width int) int {
	if width <= 0 {
		return 0
	}
	minCols := cfg.Int("DIR_MIN_COMMAND_COLUMNS", 0)
	if pct := cfg.Int("DIR_MIN_COMMAND_COLUMNS_PCT", 0); pct > 0 {
		if fromPct := width * pct / 100; fromPct > minCols {
			minCols = fromPct
		}
	}
	return minCols
}
