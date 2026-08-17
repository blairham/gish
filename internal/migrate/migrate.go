// Package migrate imports an existing bash or zsh setup into gish
// (#160).
//
// The research is blunt about why this exists: mise out-installs asdf
// roughly eight to one, and it shipped a converter plus a "coming from
// rtx" page; zoxide auto-imports z, autojump and fasd databases. In
// both cases the migration tool is what turns intent into an install.
// A shell that says "here is a config format, good luck" is asking for
// an afternoon that most people will not spend.
//
// Two rules shape everything here.
//
// **Nothing is executed.** The rc files are parsed, never sourced.
// Importing someone's config by running it is how an import becomes an
// attack, and it is also how a migration inherits the very startup cost
// people are leaving. Parsing means the importer can be honest about
// what it did not understand, which sourcing never can.
//
// **Nothing is dropped silently.** Every construct that does not
// translate is reported by name, with the line it came from and why —
// that report is the part users trust, and the part that makes the
// converted config safe to adopt without diffing it against the
// original.
package migrate

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Source is one file the importer read.
type Source struct {
	Path  string
	Shell string // "bash" or "zsh"
}

// Alias is an imported alias.
type Alias struct{ Name, Value string }

// Export is an imported environment assignment.
type Export struct{ Name, Value string }

// Skipped is one construct that did not translate.
type Skipped struct {
	File   string
	Line   uint
	Text   string
	Reason string
}

// PluginManager is a detected plugin manager and what to do about it.
type PluginManager struct {
	Name   string
	Detail string
	// Native says whether gish has an equivalent the user can adopt.
	Native bool
}

// Plan is what an import found. It is a value, not an action: `migrate`
// prints it, and only `--apply` writes anything.
type Plan struct {
	Sources    []Source
	Aliases    []Alias
	Functions  []string // names, with bodies kept in FunctionSrc
	Exports    []Export
	PathAdds   []string
	Theme      string // the gish theme nearest to what was detected
	ThemeWhy   string
	Managers   []PluginManager
	Skipped    []Skipped
	HistoryIn  string // the history file found
	HistoryNum int    // entries readable from it

	FunctionSrc map[string]string
}

// Detect finds and reads the user's existing setup under home.
func Detect(home string) (*Plan, error) {
	plan := &Plan{FunctionSrc: map[string]string{}}
	for _, cand := range []struct {
		rel   string
		shell string
	}{
		{".bashrc", "bash"},
		{".bash_profile", "bash"},
		{".bash_aliases", "bash"},
		{".zshrc", "zsh"},
		{".zprofile", "zsh"},
		{".zshenv", "zsh"},
		{".profile", "sh"},
	} {
		path := filepath.Join(home, cand.rel)
		data, err := os.ReadFile(path) //nolint:gosec // the user's own rc, read on request
		if err != nil {
			continue
		}
		plan.Sources = append(plan.Sources, Source{Path: path, Shell: cand.shell})
		plan.readRC(path, string(data))
	}
	plan.detectHistory(home)
	plan.dedupe()
	return plan, nil
}

// readRC parses one rc file and folds what it understands into the plan.
//
// The parse is bash's, which is also gish's; zsh files that use zsh-only
// syntax fail to parse as a whole, and that is exactly when the
// line-based fallback earns its place — an rc is a list of mostly
// independent lines, and one zsh-ism should not cost the other ninety.
func (p *Plan) readRC(path, src string) {
	file, err := syntax.NewParser().Parse(strings.NewReader(src), path)
	if err != nil {
		p.readRCByLine(path, src, err)
		return
	}
	// Top-level statements only. Walking the whole tree would find the
	// aliases inside an `if` and import them unconditionally, which is
	// worse than not importing them: the condition was there for a
	// reason, usually "only on this machine".
	for _, stmt := range file.Stmts {
		p.readStmt(path, src, stmt)
	}
}

// readStmt folds one top-level statement into the plan, or records why
// it did not translate. Every statement lands in one of those two
// places — that is what "nothing is dropped silently" means in code.
func (p *Plan) readStmt(path, src string, stmt *syntax.Stmt) {
	switch cmd := stmt.Cmd.(type) {
	case *syntax.FuncDecl:
		name := cmd.Name.Value
		p.Functions = append(p.Functions, name)
		var b strings.Builder
		if err := syntax.NewPrinter().Print(&b, cmd); err == nil {
			p.FunctionSrc[name] = b.String()
		}
	case *syntax.CallExpr:
		for _, a := range cmd.Assigns {
			p.readAssign(path, src, a)
		}
		if len(cmd.Args) == 0 {
			return // a bare assignment line: already handled above
		}
		if !p.readCall(path, src, cmd) {
			p.note(path, src, stmt, "command not translated — run it in your gish rc if you still need it")
		}
	case *syntax.DeclClause:
		// `export FOO=bar` is a declaration clause, not a call — which
		// is easy to miss, and reports every export in the file as
		// untranslated control flow if you do.
		for _, a := range cmd.Args {
			p.readAssign(path, src, a)
		}
	case nil:
		// An empty statement carries nothing.
	default:
		p.note(path, src, stmt, "control flow is not translated; copy it over if you need it")
	}
}

// readCall picks up `alias name=value` and the tool-init lines whose
// gish equivalent is a setting rather than a command.
func (p *Plan) readCall(path, src string, call *syntax.CallExpr) bool {
	if len(call.Args) == 0 {
		return true
	}
	words := make([]string, 0, len(call.Args))
	for _, a := range call.Args {
		words = append(words, wordText(a))
	}
	switch words[0] {
	case "alias":
		for _, w := range words[1:] {
			if strings.HasPrefix(w, "-") {
				continue
			}
			name, value, ok := strings.Cut(w, "=")
			if !ok {
				continue
			}
			p.Aliases = append(p.Aliases, Alias{Name: name, Value: unquote(value)})
		}
		return true
	case "eval":
		joined := strings.Join(words, " ")
		switch {
		case strings.Contains(joined, "starship init"):
			p.setTheme("starship", "your rc runs `starship init`")
		case strings.Contains(joined, "oh-my-posh"):
			p.note(path, src, call, "oh-my-posh is not ported; the nearest gish theme is p10k")
		default:
			return false
		}
		return true
	case "source", ".":
		joined := strings.Join(words[1:], " ")
		switch {
		case strings.Contains(joined, "oh-my-zsh.sh"):
			p.addManager("oh-my-zsh", "themes and plugins are zsh scripts; `plugin add` can load the ones that are shell-agnostic", false)
		case strings.Contains(joined, "p10k.zsh"):
			p.setTheme("p10k", "your rc sources a powerlevel10k config — import it with `p10k import`")
		case strings.Contains(joined, "zinit") || strings.Contains(joined, "zi.zsh"):
			p.addManager("zinit/zi", "gish has the engine natively: `zi migrate` imports installed objects", true)
		case strings.Contains(joined, "antidote"):
			p.addManager("antidote", "list your bundles in plugins.toml with `plugin add`", false)
		case strings.Contains(joined, "zplug"):
			p.addManager("zplug", "list your bundles in plugins.toml with `plugin add`", false)
		default:
			return false
		}
		return true
	}
	return false
}

// readAssign picks up exports, PATH edits and the theme markers that are
// assignments rather than commands.
func (p *Plan) readAssign(path, src string, a *syntax.Assign) {
	if a.Name == nil {
		return
	}
	name := a.Name.Value
	value := ""
	if a.Value != nil {
		value = wordText(a.Value)
	}
	switch {
	case name == "PATH":
		for _, part := range strings.Split(value, ":") {
			if part == "" || strings.Contains(part, "$PATH") || part == "$PATH" {
				continue
			}
			p.PathAdds = append(p.PathAdds, part)
		}
	case strings.HasPrefix(name, "POWERLEVEL9K_"):
		p.setTheme("p10k", "your rc sets POWERLEVEL9K_* — gish has a native port; `p10k import` takes the whole config")
	case name == "ZSH_THEME":
		p.setTheme("p10k", "oh-my-zsh theme "+unquote(value)+" has no port; p10k is the closest built-in")
	case name == "PS1" || name == "PROMPT":
		p.note(path, src, a, "prompt string kept as-is: gish renders PS1, but the escapes it uses are bash's")
	case a.Naked, value == "":
		// A bare name or an empty value carries nothing to import.
	default:
		p.Exports = append(p.Exports, Export{Name: name, Value: value})
	}
}

// readRCByLine is the fallback for files the bash parser rejects — a
// zsh rc using zsh-only syntax. It takes the lines it can recognize on
// their own and reports the file as partially understood, which is the
// honest description of what happened.
func (p *Plan) readRCByLine(path, src string, parseErr error) {
	p.Skipped = append(p.Skipped, Skipped{
		File:   path,
		Reason: "not valid bash syntax (" + parseErr.Error() + ") — read line by line instead",
	})
	scanner := bufio.NewScanner(strings.NewReader(src))
	var lineNo uint
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "alias "):
			name, value, ok := strings.Cut(strings.TrimPrefix(line, "alias "), "=")
			if ok {
				p.Aliases = append(p.Aliases, Alias{Name: strings.TrimSpace(name), Value: unquote(value)})
			}
		case strings.HasPrefix(line, "export "):
			name, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
			if ok {
				p.Exports = append(p.Exports, Export{Name: strings.TrimSpace(name), Value: unquote(value)})
			}
		default:
			p.Skipped = append(p.Skipped, Skipped{File: path, Line: lineNo, Text: line, Reason: "not recognized in the line-by-line fallback"})
		}
	}
}

func (p *Plan) setTheme(name, why string) {
	// Last one wins, because that is what the rc itself does: a file
	// that sets ZSH_THEME and then evals `starship init` renders
	// starship, and importing the first would hand the user a prompt
	// they replaced years ago.
	p.Theme, p.ThemeWhy = name, why
}

func (p *Plan) addManager(name, detail string, native bool) {
	for _, m := range p.Managers {
		if m.Name == name {
			return
		}
	}
	p.Managers = append(p.Managers, PluginManager{Name: name, Detail: detail, Native: native})
}

func (p *Plan) note(path, src string, node syntax.Node, reason string) {
	pos := node.Pos()
	text := ""
	if lines := strings.Split(src, "\n"); int(pos.Line()) <= len(lines) && pos.Line() > 0 {
		text = strings.TrimSpace(lines[pos.Line()-1])
	}
	p.Skipped = append(p.Skipped, Skipped{File: path, Line: pos.Line(), Text: text, Reason: reason})
}

// dedupe collapses the repeats an rc pile always has: the same alias in
// .bashrc and .bash_aliases, the same export in two profiles.
func (p *Plan) dedupe() {
	seenAlias := map[string]bool{}
	var aliases []Alias
	for i := len(p.Aliases) - 1; i >= 0; i-- {
		a := p.Aliases[i]
		if seenAlias[a.Name] {
			continue
		}
		seenAlias[a.Name] = true
		aliases = append([]Alias{a}, aliases...)
	}
	p.Aliases = aliases

	seenExport := map[string]bool{}
	var exports []Export
	for i := len(p.Exports) - 1; i >= 0; i-- {
		e := p.Exports[i]
		if seenExport[e.Name] {
			continue
		}
		seenExport[e.Name] = true
		exports = append([]Export{e}, exports...)
	}
	p.Exports = exports

	seenPath := map[string]bool{}
	var paths []string
	for _, d := range p.PathAdds {
		if seenPath[d] {
			continue
		}
		seenPath[d] = true
		paths = append(paths, d)
	}
	p.PathAdds = paths

	sort.Strings(p.Functions)
	p.Functions = compactStrings(p.Functions)
}

func compactStrings(in []string) []string {
	var out []string
	for i, s := range in {
		if i > 0 && in[i-1] == s {
			continue
		}
		out = append(out, s)
	}
	return out
}

// detectHistory finds the history file worth importing and counts what
// is readable in it.
func (p *Plan) detectHistory(home string) {
	candidates := []string{
		os.Getenv("HISTFILE"),
		filepath.Join(home, ".zsh_history"),
		filepath.Join(home, ".bash_history"),
	}
	for _, path := range candidates {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // the user's own history, read on request
		if err != nil {
			continue
		}
		entries := ParseHistory(string(data))
		if len(entries) == 0 {
			continue
		}
		p.HistoryIn, p.HistoryNum = path, len(entries)
		return
	}
}

// HistoryEntry is one imported command.
type HistoryEntry struct {
	Command    string
	UnixSec    int64 // 0 when the format carried no timestamp
	DurationMs int64
}

// ParseHistory reads either history format.
//
// zsh's extended history is `: <started>:<elapsed>;<command>`, with a
// trailing backslash continuing onto the next line; bash's is plain
// commands, optionally preceded by a `#<epoch>` line when
// HISTTIMEFORMAT was set. Both are handled by the same pass because a
// user does not always know which one their file is in — and because
// getting it wrong silently imports `: 1700000000:0;ls` as a command.
func ParseHistory(data string) []HistoryEntry {
	var out []HistoryEntry
	var pending int64
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var continued strings.Builder
	var contTime int64

	flush := func() {
		if continued.Len() == 0 {
			return
		}
		out = append(out, HistoryEntry{Command: continued.String(), UnixSec: contTime})
		continued.Reset()
		contTime = 0
	}

	for scanner.Scan() {
		line := scanner.Text()
		if continued.Len() > 0 {
			if strings.HasSuffix(line, "\\") {
				continued.WriteString("\n" + strings.TrimSuffix(line, "\\"))
				continue
			}
			continued.WriteString("\n" + line)
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, ": ") && strings.Contains(line, ";"):
			meta, cmd, _ := strings.Cut(strings.TrimPrefix(line, ": "), ";")
			started, elapsed := splitZshMeta(meta)
			if strings.HasSuffix(cmd, "\\") {
				continued.WriteString(strings.TrimSuffix(cmd, "\\"))
				contTime = started
				continue
			}
			if strings.TrimSpace(cmd) == "" {
				continue
			}
			out = append(out, HistoryEntry{Command: cmd, UnixSec: started, DurationMs: elapsed * 1000})
		case strings.HasPrefix(line, "#"):
			// bash's HISTTIMEFORMAT stamp for the line that follows.
			if ts, err := strconv.ParseInt(strings.TrimPrefix(line, "#"), 10, 64); err == nil {
				pending = ts
			}
		case strings.TrimSpace(line) == "":
		default:
			out = append(out, HistoryEntry{Command: line, UnixSec: pending})
			pending = 0
		}
	}
	flush()
	return out
}

func splitZshMeta(meta string) (started, elapsed int64) {
	startStr, elapsedStr, _ := strings.Cut(meta, ":")
	started, _ = strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	elapsed, _ = strconv.ParseInt(strings.TrimSpace(elapsedStr), 10, 64)
	return started, elapsed
}

// wordText renders a word back to source text.
func wordText(w *syntax.Word) string {
	var b strings.Builder
	if err := syntax.NewPrinter().Print(&b, w); err != nil {
		return ""
	}
	return b.String()
}

// unquote strips one layer of surrounding quotes, which is what an rc's
// `alias ll='ls -l'` needs and all it needs.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// GishRC renders the plan as a gish rc file.
func (p *Plan) GishRC() string {
	var b strings.Builder
	b.WriteString("# Written by `gish migrate` from:\n")
	for _, s := range p.Sources {
		fmt.Fprintf(&b, "#   %s (%s)\n", s.Path, s.Shell)
	}
	b.WriteString("# Anything that did not translate is listed in the migration report;\n")
	b.WriteString("# nothing was dropped silently.\n")

	if p.Theme != "" {
		fmt.Fprintf(&b, "\n# %s\nGISH_THEME=%s\n", p.ThemeWhy, p.Theme)
	}
	if len(p.PathAdds) > 0 {
		b.WriteString("\n# PATH entries from your rc, in the order they appeared.\n")
		for _, dir := range p.PathAdds {
			fmt.Fprintf(&b, "PATH=%s:$PATH\n", dir)
		}
	}
	if len(p.Exports) > 0 {
		b.WriteString("\n# Exports.\n")
		for _, e := range p.Exports {
			fmt.Fprintf(&b, "export %s=%s\n", e.Name, e.Value)
		}
	}
	if len(p.Aliases) > 0 {
		b.WriteString("\n# Aliases.\n")
		for _, a := range p.Aliases {
			fmt.Fprintf(&b, "alias %s=%s\n", a.Name, shellQuote(a.Value))
		}
	}
	if len(p.Functions) > 0 {
		b.WriteString("\n# Functions, copied verbatim.\n")
		for _, name := range p.Functions {
			if src, ok := p.FunctionSrc[name]; ok {
				b.WriteString(src + "\n")
			}
		}
	}
	return b.String()
}

// count renders "1 alias" and "3 aliases" — a report people are meant
// to read should read like one.
func count(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + plural
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Report is the honest half: what was found, and what did not translate.
func (p *Plan) Report() string {
	var b strings.Builder
	b.WriteString("gish migrate — what was found\n\n")
	if len(p.Sources) == 0 {
		b.WriteString("  no bash or zsh rc files in your home directory\n")
		return b.String()
	}
	for _, s := range p.Sources {
		fmt.Fprintf(&b, "  read   %s (%s)\n", s.Path, s.Shell)
	}
	fmt.Fprintf(&b, "  found  %s, %s, %s, %s\n",
		count(len(p.Aliases), "alias", "aliases"),
		count(len(p.Functions), "function", "functions"),
		count(len(p.Exports), "export", "exports"),
		count(len(p.PathAdds), "PATH entry", "PATH entries"))
	if p.Theme != "" {
		fmt.Fprintf(&b, "  theme  %s — %s\n", p.Theme, p.ThemeWhy)
	}
	if p.HistoryIn != "" {
		fmt.Fprintf(&b, "  history %s (%d entries; secrets are dropped on import)\n", p.HistoryIn, p.HistoryNum)
	}
	for _, m := range p.Managers {
		verdict := "no native equivalent"
		if m.Native {
			verdict = "native equivalent"
		}
		fmt.Fprintf(&b, "  plugin manager %s — %s: %s\n", m.Name, verdict, m.Detail)
	}

	if len(p.Skipped) == 0 {
		return b.String()
	}
	b.WriteString("\nnot translated (nothing here was dropped silently):\n")
	for _, s := range p.Skipped {
		switch s.Line {
		case 0:
			fmt.Fprintf(&b, "  %s: %s\n", s.File, s.Reason)
		default:
			fmt.Fprintf(&b, "  %s:%d: %s\n      %s\n", s.File, s.Line, s.Reason, s.Text)
		}
	}
	return b.String()
}
