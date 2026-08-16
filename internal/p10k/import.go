package p10k

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Importing an existing .p10k.zsh.
//
// This is the one place in the package that looks at zsh, and it is
// deliberately not on any hot path: `p10k import` runs once, reads the
// declarative settings out of a configuration file, and writes them to
// the native config. After that the zsh file is never consulted again.
//
// What comes across is the ~500 POWERLEVEL9K_* assignments, which are
// data. What cannot come across is the code: a stock .p10k.zsh defines a
// shell function (my_git_formatter) and points VCS_CONTENT_EXPANSION at
// it, and honoring that would mean running zsh on the prompt path. Those
// settings are reported by name instead of being silently dropped or,
// worse, half-interpreted into something that looks nearly right.
//
// The parser is purpose-built rather than a shell parser because a
// general one cannot help here: every .p10k.zsh is wrapped in a zsh
// anonymous function, which no bash parser will accept. The file's real
// grammar is much smaller than zsh — assignments, arrays, comments — and
// that is what this reads.

// DefaultZshConfigPath is where p10k configure writes by default.
func DefaultZshConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".p10k.zsh"), nil
}

// assignRe matches an assignment, after any leading declaration keyword
// and flags have been stripped. The name may contain brace expansions,
// which presets use heavily to set four settings on one line.
var assignRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*|[A-Za-z_{][A-Za-z0-9_{},]*}*)=(.*)$`)

// declKeywords are the words that may precede an assignment.
var declKeywords = map[string]bool{
	"typeset": true, "declare": true, "export": true, "local": true, "readonly": true,
}

// ImportZshConfig reads a .p10k.zsh and returns the settings it could
// take, along with a report of what it could not.
func ImportZshConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg := importFrom(f)
	cfg.Sources = append(cfg.Sources, path)
	return cfg, nil
}

func importFrom(r io.Reader) *Config {
	cfg := NewConfig()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		arrayNames []string
		arrayItems []string
		inArray    bool
	)

	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := scanner.Text()

		if inArray {
			body, closed := cutArrayEnd(line)
			arrayItems = append(arrayItems, splitWords(body)...)
			if closed {
				for _, name := range arrayNames {
					cfg.SetList(name, arrayItems)
				}
				inArray, arrayNames, arrayItems = false, nil, nil
			}
			continue
		}

		stmt := stripDeclaration(strings.TrimSpace(line))
		if stmt == "" || strings.HasPrefix(stmt, "#") {
			continue
		}
		m := assignRe.FindStringSubmatch(stmt)
		if m == nil {
			continue // not an assignment: functions, conditionals, calls
		}
		names := expandBraces(m[1])
		names = keepPowerlevel(names)
		if len(names) == 0 {
			continue
		}
		value := m[2]

		if rest, isArray := strings.CutPrefix(strings.TrimSpace(value), "("); isArray {
			body, closed := cutArrayEnd(rest)
			items := splitWords(body)
			if !closed {
				inArray, arrayNames, arrayItems = true, names, items
				continue
			}
			for _, name := range names {
				cfg.SetList(name, items)
			}
			continue
		}

		scalar, ok := unquote(value)
		if !ok {
			cfg.Unsupported = append(cfg.Unsupported,
				fmt.Sprintf("line %d: %s (value could not be read literally)", lineNo, names[0]))
			continue
		}
		if why, unsupported := unsupportedValue(scalar); unsupported {
			cfg.Unsupported = append(cfg.Unsupported,
				fmt.Sprintf("line %d: %s (%s)", lineNo, names[0], why))
			continue
		}
		for _, name := range names {
			cfg.Set(name, scalar)
		}
	}
	return cfg
}

// keepPowerlevel drops anything that is not one of ours, and returns the
// remainder with the prefix stripped.
func keepPowerlevel(names []string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, "POWERLEVEL9K_") || strings.HasPrefix(n, "POWERLEVEL10K_") {
			out = append(out, normKey(n))
		}
	}
	return out
}

// stripDeclaration removes a leading declaration keyword and its flags,
// so `typeset -g FOO=bar` reads as `FOO=bar`.
func stripDeclaration(line string) string {
	for {
		word, rest, found := strings.Cut(line, " ")
		if !found || !declKeywords[word] {
			return line
		}
		line = strings.TrimSpace(rest)
		// Drop flags belonging to the keyword.
		for strings.HasPrefix(line, "-") {
			_, after, more := strings.Cut(line, " ")
			if !more {
				return ""
			}
			line = strings.TrimSpace(after)
		}
	}
}

// expandBraces turns POWERLEVEL9K_{LEFT,RIGHT}_WHITESPACE into both
// names. Presets set four related settings on one line this way, so
// without this an import would quietly lose most of a layout.
func expandBraces(name string) []string {
	open := strings.IndexByte(name, '{')
	if open < 0 {
		return []string{name}
	}
	shut := strings.IndexByte(name[open:], '}')
	if shut < 0 {
		return []string{name}
	}
	shut += open

	prefix, alternatives, suffix := name[:open], name[open+1:shut], name[shut+1:]
	var out []string
	for _, alt := range strings.Split(alternatives, ",") {
		out = append(out, expandBraces(prefix+alt+suffix)...)
	}
	return out
}

// stripComment removes a trailing comment, respecting quotes.
//
// This has to come before anything else looks at a line. Upstream
// annotates element lists item by item, and those comments contain
// parentheses — "direnv # direnv status (https://direnv.net/)". Hunting
// for the array's closing paren first finds the one in a URL, and the
// list silently ends four items in.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '('):
			return s[:i]
		}
	}
	return s
}

// cutArrayEnd splits an array body at its closing parenthesis, reporting
// whether it was found on this line. Comments are removed first.
func cutArrayEnd(s string) (body string, closed bool) {
	s = stripComment(s)
	if i := strings.IndexByte(s, ')'); i >= 0 {
		return s[:i], true
	}
	return s, false
}

// splitWords splits an array body into elements. Each element is
// unquoted; comments have already been removed by cutArrayEnd.
func splitWords(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		if v, ok := unquote(f); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// unquote reads one shell word literally. ok is false when the value
// depends on evaluation rather than on the text.
func unquote(raw string) (string, bool) {
	s := strings.TrimSpace(stripComment(raw))
	switch {
	case s == "":
		// Includes `KEY=  # a comment`, which sets the key to empty —
		// and an explicitly empty separator is a real setting, not a
		// missing one.
		return "", true
	case strings.HasPrefix(s, "$'"):
		if end := strings.LastIndexByte(s, '\''); end > 1 {
			return decodeEscapes(s[2:end]), true
		}
		return "", false
	case strings.HasPrefix(s, "'"):
		if end := strings.LastIndexByte(s, '\''); end > 0 {
			return s[1:end], true
		}
		return "", false
	case strings.HasPrefix(s, `"`):
		if end := strings.LastIndexByte(s, '"'); end > 0 {
			return s[1:end], true
		}
		return "", false
	}
	// Bare word: ends at the first whitespace.
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", true
	}
	return fields[0], true
}

// unsupportedValue recognizes values whose meaning is code rather than
// data, and says so in the terms the user will recognize.
func unsupportedValue(v string) (string, bool) {
	switch {
	case strings.Contains(v, "$(("):
		return "arithmetic, or a shell function called through it", true
	case strings.Contains(v, "$("), strings.Contains(v, "`"):
		return "runs a command", true
	case strings.Contains(v, "${") && strings.ContainsAny(v, "()"):
		return "calls a shell function", true
	}
	return "", false
}
