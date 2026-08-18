// Package complete implements koi's core completion providers: command
// names and file paths. Per docs/plugins.md's dividing rule these are
// core (pure-local, keystroke-class); plugin providers merge in behind
// their latency budget at the repl layer.
package complete

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Candidate is one completion. Value replaces the word being completed
// (unescaped — the editor owns quoting on insertion); Display is what
// candidate lists show.
type Candidate struct {
	Value   string
	Display string
}

// Result is a completion answer: candidates for the word starting at
// WordStart (a rune index into the buffer).
type Result struct {
	WordStart  int
	Candidates []Candidate
}

// Analyze finds the word being completed at cursor and whether it sits
// in command position (first word of the line or after a separator).
// Quote handling is deliberately simple for v1: a leading quote on the
// current word is stripped.
func Analyze(text string, cursor int) (word string, wordStart int, isCommand bool) {
	runes := []rune(text)
	if cursor > len(runes) {
		cursor = len(runes)
	}
	start := cursor
	for start > 0 && !isWordBreak(runes[start-1]) {
		start--
	}
	word = string(runes[start:cursor])
	word = strings.TrimPrefix(word, `"`)
	word = strings.TrimPrefix(word, `'`)

	// Command position: nothing but whitespace or a command separator
	// before the word.
	i := start - 1
	for i >= 0 && (runes[i] == ' ' || runes[i] == '\t') {
		i--
	}
	isCommand = i < 0 || strings.ContainsRune(";|&(\n", runes[i])
	return word, start, isCommand
}

func isWordBreak(r rune) bool {
	switch r {
	case ' ', '\t', '\n', ';', '|', '&', '(', ')', '<', '>':
		return true
	}
	return false
}

// Files completes file paths for word: prefix match in the word's
// directory, ~-expansion, trailing / on directories, hidden entries only
// when the prefix asks for them.
func Files(word, cwd string) []Candidate {
	dirPart, prefix := filepath.Split(word)
	scanDir := dirPart

	// ~ expansion for scanning; the completed value keeps the ~ form.
	if strings.HasPrefix(scanDir, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			scanDir = home + strings.TrimPrefix(scanDir, "~")
		}
	}
	switch {
	case scanDir == "":
		scanDir = cwd
	case !filepath.IsAbs(scanDir):
		scanDir = filepath.Join(cwd, scanDir)
	}

	entries, err := os.ReadDir(scanDir)
	if err != nil {
		return nil
	}
	showHidden := strings.HasPrefix(prefix, ".")
	var out []Candidate
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		value := dirPart + name
		display := name
		if isDir(e, filepath.Join(scanDir, name)) {
			value += "/"
			display += "/"
		}
		out = append(out, Candidate{Value: value, Display: display})
	}
	sortCandidates(out)
	return out
}

// isDir resolves symlinks so a link to a directory completes like one.
func isDir(e os.DirEntry, path string) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink != 0 {
		if st, err := os.Stat(path); err == nil {
			return st.IsDir()
		}
	}
	return false
}

// Commands completes command names: PATH executables, the provided
// builtin/function names, deduped.
func Commands(word, pathVar string, extra []string) []Candidate {
	seen := map[string]bool{}
	var out []Candidate
	add := func(name string) {
		if strings.HasPrefix(name, word) && !seen[name] {
			seen[name] = true
			out = append(out, Candidate{Value: name, Display: name})
		}
	}
	for _, name := range extra {
		add(name)
	}
	for _, name := range pathExecutables(pathVar) {
		add(name)
	}
	sortCandidates(out)
	return out
}

// pathCache memoizes the PATH scan per PATH value: completing shouldn't
// re-stat every directory on every Tab.
var pathCache sync.Map // pathVar → []string

func pathExecutables(pathVar string) []string {
	if v, ok := pathCache.Load(pathVar); ok {
		return v.([]string)
	}
	seen := map[string]bool{}
	var names []string
	for _, dir := range filepath.SplitList(pathVar) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name, ok := executableName(e)
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	slices.Sort(names)
	pathCache.Store(pathVar, names)
	return names
}

func sortCandidates(cs []Candidate) {
	slices.SortFunc(cs, func(a, b Candidate) int {
		return strings.Compare(a.Value, b.Value)
	})
}

// IsCommand reports whether name resolves as a command: a PATH
// executable (cached per PATH value). Builtins and functions are the
// caller's to check — they live in the shell, not here.
func IsCommand(name, pathVar string) bool {
	_, ok := slices.BinarySearch(pathExecutables(pathVar), name)
	return ok
}
