package main

import (
	"os"
	"path/filepath"
	"strings"
)

// The .env dialect, and why it is spelled out here: .env has no spec.
// What it has is two de-facto references almost every loader copies —
// motdotla/dotenv (node) and docker compose — and this parser follows
// the rules the two agree on:
//
//   - blank lines and lines whose first non-space character is `#` are
//     comments
//   - an optional `export ` prefix (so a file also usable via `source`
//     parses the same way here)
//   - names are [A-Za-z_][A-Za-z0-9_]*; whitespace is allowed around `=`
//   - single-quoted values are literal; double-quoted values decode
//     \n \r \t \" \\ (an unrecognized escape keeps both characters);
//     either kind may span lines
//   - an unquoted value runs to end of line, with a whitespace-preceded
//     `#` starting a trailing comment — `a#b` is a value, `a #b` is "a"
//   - a later duplicate wins, matching sequential assignment everywhere
//
// Where the references disagree or a rule would mean interpreting the
// file, this parser refuses rather than guesses: no interpolation ($VAR
// is the literal string), no command execution, and a malformed line is
// skipped so one typo costs one variable, not the file. The one
// exception to line-scoped recovery is an unterminated quote — the rest
// of the file is inside the string, so "parsing on" would manufacture
// variables out of string content, and parsing stops instead.

// maxDotenvSize bounds what load will read. .env files are hand-written
// config; anything this large is a mistake (or not a dotenv file at
// all), and proposing a megabyte of "variables" through the trust
// prompt helps nobody.
const maxDotenvSize = 1 << 20

// findDotenv walks up from dir and returns the nearest regular file
// named .env, or "". Walking up is direnv's own scope rule, and it is
// what makes for_dir-keyed trust cover a subtree the way users expect.
//
// Non-regular .env entries are skipped and the walk continues: a
// directory named .env is a common virtualenv location (`python -m venv
// .env`), and stopping there would hide a real parent .env behind it.
func findDotenv(dir string) string {
	for {
		p := filepath.Join(dir, ".env")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// load reads and parses path. An oversized file is an error rather than
// a truncated parse — a proposal built from half a file would be a
// quiet lie about what the file sets.
func load(path string) (map[string]string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxDotenvSize {
		return nil, os.ErrInvalid
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(string(data)), nil
}

// parse implements the dialect documented at the top of this file.
func parse(src string) map[string]string {
	out := map[string]string{}
	src = strings.TrimPrefix(src, "\uFEFF")
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			trimmed = strings.TrimLeft(rest, " \t")
		}
		name, rawValue, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		name = strings.TrimRight(name, " \t")
		if !validName(name) {
			continue
		}
		value, next, terminated := parseValue(strings.TrimLeft(rawValue, " \t"), lines, i)
		if !terminated {
			// The rest of the file is inside an unclosed quote; anything
			// "parsed" from it would be string content wearing an = sign.
			break
		}
		i = next
		out[name] = value
	}
	return out
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// parseValue reads a value starting at raw (the text after `=` on line
// i of lines). It returns the decoded value, the index of the line the
// value ended on, and whether a quoted value was actually terminated.
func parseValue(raw string, lines []string, i int) (value string, endLine int, terminated bool) {
	if raw == "" {
		return "", i, true
	}
	quote := raw[0]
	if quote != '\'' && quote != '"' {
		return unquoted(raw), i, true
	}

	var b strings.Builder
	rest := raw[1:]
	for {
		idx := closingQuote(rest, quote)
		if idx >= 0 {
			b.WriteString(rest[:idx])
			// Anything after the closing quote on the line is ignored —
			// in practice a trailing comment.
			break
		}
		b.WriteString(rest)
		i++
		if i >= len(lines) {
			return "", i - 1, false
		}
		b.WriteByte('\n')
		rest = strings.TrimRight(lines[i], "\r")
	}
	if quote == '\'' {
		return b.String(), i, true
	}
	return decodeEscapes(b.String()), i, true
}

// closingQuote finds the index of the closing quote in s, honoring
// backslash escapes inside double quotes only.
func closingQuote(s string, quote byte) int {
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case '\\':
			if quote == '"' {
				j++ // the escaped character cannot close the string
			}
		case quote:
			return j
		}
	}
	return -1
}

// unquoted takes the rest of the line, cutting a whitespace-preceded
// trailing comment and trimming surrounding whitespace.
func unquoted(raw string) string {
	for j := 1; j < len(raw); j++ {
		if raw[j] == '#' && (raw[j-1] == ' ' || raw[j-1] == '\t') {
			raw = raw[:j]
			break
		}
	}
	return strings.TrimSpace(raw)
}

// decodeEscapes handles the double-quote escapes both reference
// implementations agree on. An unrecognized escape keeps the backslash
// and the character: guessing an interpretation for \d would corrupt
// exactly the values (regexes, Windows paths) people quote to protect.
func decodeEscapes(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for j := 0; j < len(s); j++ {
		if s[j] != '\\' || j+1 >= len(s) {
			b.WriteByte(s[j])
			continue
		}
		j++
		switch s[j] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[j])
		}
	}
	return b.String()
}
