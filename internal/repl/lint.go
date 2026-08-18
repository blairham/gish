package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Footgun diagnostics (#46). bash inherits its quoting and
// word-splitting semantics on purpose (compatibility > purity), but koi
// holds something bash never did: a real parse tree of the line before
// it runs. These checks walk that tree on every keystroke and surface
// dim caution lines below the edit line — the same surface as completion
// candidates. Warnings never block execution; KOI_LINT is the knob:
//
//	on      (default) native checks + shellcheck on Enter for multi-line
//	native  native checks only, never spawn shellcheck
//	off     no diagnostics at all
//
// The checks are deliberately the top real-world foot-blowers, not a
// linter: unquoted expansion under a destructive command, cd sequenced
// (not &&-chained) before a destructive command, unquoted expansion in
// [ ], useless cat, and a redirect that truncates the file a command is
// about to read.

const (
	lintStyle = "\x1b[2;33m" // caution line: dim yellow

	// maxNativeWarnings keeps the caution surface small: the worst line
	// gets its top few problems, not a lecture.
	maxNativeWarnings = 3
)

// lintFn builds the editor's Diagnose hook. The knob is read per call so
// KOI_LINT=off set interactively takes effect on the next keystroke.
func lintFn(runner *interp.Runner, color bool) func(string) []string {
	return func(text string) []string {
		if lintMode(runner) == "off" {
			return nil
		}
		warns := footgunWarnings(text)
		for i, w := range warns {
			if color {
				warns[i] = lintStyle + "⚠ " + w + cReset
			} else {
				warns[i] = "warning: " + w
			}
		}
		return warns
	}
}

// lintMode resolves KOI_LINT to one of on|native|off.
func lintMode(runner *interp.Runner) string {
	switch m := shellVar(runner, "KOI_LINT", "on"); m {
	case "off", "native":
		return m
	default:
		return "on"
	}
}

// footgunWarnings parses text and reports the native checks. A parse
// error means the line is mid-edit: no warnings rather than noise.
func footgunWarnings(text string) []string {
	file, err := syntax.NewParser().Parse(strings.NewReader(text), "")
	if err != nil {
		return nil
	}

	var warns []string
	add := func(format string, args ...any) {
		w := fmt.Sprintf(format, args...)
		if !slices.Contains(warns, w) {
			warns = append(warns, w)
		}
	}

	checkSequence(file.Stmts, add)
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Block:
			checkSequence(n.Stmts, add)
		case *syntax.Subshell:
			checkSequence(n.Stmts, add)
		case *syntax.IfClause:
			checkSequence(n.Then, add)
		case *syntax.WhileClause:
			checkSequence(n.Do, add)
		case *syntax.ForClause:
			checkSequence(n.Do, add)
		case *syntax.Stmt:
			checkClobber(n, add)
		case *syntax.CallExpr:
			checkCall(n, add)
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe {
				checkUselessCat(n, add)
			}
		}
		return true
	})

	if len(warns) > maxNativeWarnings {
		warns = warns[:maxNativeWarnings]
	}
	return warns
}

// destructive lists commands where an argument that splits, globs, or
// vanishes does real damage instead of printing something odd.
var destructive = map[string]bool{
	"rm": true, "rmdir": true, "mv": true, "cp": true,
	"chmod": true, "chown": true, "ln": true, "dd": true, "truncate": true,
}

// safeParams are expansions that never split: numeric or single-token by
// definition. $@/$* and positionals stay flagged — they're the classics.
var safeParams = map[string]bool{"?": true, "$": true, "#": true, "!": true}

// cmdName returns a statement's leading literal command name, descending
// through && || | chains to the first command.
func cmdName(st *syntax.Stmt) string {
	switch c := st.Cmd.(type) {
	case *syntax.CallExpr:
		if len(c.Args) > 0 {
			return flatLiteral(c.Args[0])
		}
	case *syntax.BinaryCmd:
		return cmdName(c.X)
	}
	return ""
}

// bareParam returns the name of the first unquoted variable expansion in
// a word ("" if none): a ParamExp directly among the word's parts, not
// wrapped in double quotes.
func bareParam(w *syntax.Word) string {
	for _, part := range w.Parts {
		if pe, ok := part.(*syntax.ParamExp); ok && !safeParams[pe.Param.Value] {
			return pe.Param.Value
		}
	}
	return ""
}

// checkSequence flags `cd …; destructive …`: if the cd fails, the next
// command runs wherever the shell already was.
func checkSequence(stmts []*syntax.Stmt, add func(string, ...any)) {
	for i := 0; i+1 < len(stmts); i++ {
		next := cmdName(stmts[i+1])
		if cmdName(stmts[i]) == "cd" && destructive[next] {
			add("cd can fail — chain with && so %s never runs in the wrong directory", next)
		}
	}
}

// checkCall flags unquoted expansions where splitting hurts: under a
// destructive command, and inside [ ] where an empty value breaks the
// test itself.
func checkCall(n *syntax.CallExpr, add func(string, ...any)) {
	if len(n.Args) == 0 {
		return
	}
	name := flatLiteral(n.Args[0])
	switch {
	case destructive[name]:
		for _, w := range n.Args[1:] {
			if v := bareParam(w); v != "" {
				add("unquoted $%s under %s may split or glob — safer quoted: \"$%s\"", v, name, v)
				return
			}
		}
	case name == "[" || name == "test":
		for _, w := range n.Args[1:] {
			if v := bareParam(w); v != "" {
				add("unquoted $%s in [ ] breaks on empty or spaced values — quote it, or use [[ ]]", v)
				return
			}
		}
	}
}

// checkClobber flags `cmd file > file`: the redirect truncates the file
// before the command ever reads it.
func checkClobber(st *syntax.Stmt, add func(string, ...any)) {
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) < 2 {
		return
	}
	for _, r := range st.Redirs {
		if r.Op != syntax.RdrOut {
			continue
		}
		target := flatLiteral(r.Word)
		if target == "" {
			continue
		}
		for _, a := range call.Args[1:] {
			if flatLiteral(a) == target {
				add("> %s truncates it before %s reads it — write to a temp file first", target, flatLiteral(call.Args[0]))
				return
			}
		}
	}
}

// uselessCatFilters are commands that read files themselves; `cat f |
// filter` costs a process and loses the filter its filename context.
var uselessCatFilters = map[string]bool{
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "sed": true,
	"awk": true, "head": true, "tail": true, "wc": true, "sort": true,
	"uniq": true, "cut": true, "tr": true, "less": true, "more": true,
}

// checkUselessCat flags `cat file | filter …`.
func checkUselessCat(n *syntax.BinaryCmd, add func(string, ...any)) {
	call, ok := n.X.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) != 2 || len(n.X.Redirs) > 0 {
		return
	}
	if flatLiteral(call.Args[0]) != "cat" {
		return
	}
	file := flatLiteral(call.Args[1])
	if file == "" || strings.HasPrefix(file, "-") {
		return
	}
	if right := cmdName(n.Y); uselessCatFilters[right] {
		add("useless cat — %s reads files itself: %s … %s", right, right, file)
	}
}

// --- shellcheck on Enter (#46, second half) ---

// shellcheckBudget bounds the whole pass: this runs between Enter and
// execution, so it must stay under human-perception latency. A miss
// means no findings, never a stall. Variable so tests can widen it —
// exec latency under a loaded test machine is not what it measures.
var shellcheckBudget = 200 * time.Millisecond

// maxShellcheckWarnings caps output: a pathological paste shouldn't
// scroll the terminal with lint.
const maxShellcheckWarnings = 5

// shellcheckWarnings runs the user's shellcheck (when installed) over a
// multi-line buffer and formats its warning+error findings with codes.
// Any failure — no binary, timeout, bad JSON — is silently no findings:
// this is advice, not a gate.
func shellcheckWarnings(ctx context.Context, src string) []string {
	path, err := exec.LookPath("shellcheck")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, shellcheckBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--shell=bash", "--format=json", "-")
	cmd.Stdin = strings.NewReader(src)
	out, _ := cmd.Output() //nolint:errcheck // nonzero exit just means findings; JSON is still on stdout

	var findings []struct {
		Line    int    `json:"line"`
		Column  int    `json:"column"`
		Level   string `json:"level"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(out, &findings) != nil {
		return nil
	}
	var warns []string
	for _, f := range findings {
		if f.Level != "warning" && f.Level != "error" {
			continue // severity model v1: info/style stay quiet
		}
		warns = append(warns, fmt.Sprintf("koi: shellcheck SC%d at %d:%d: %s", f.Code, f.Line, f.Column, f.Message))
		if len(warns) == maxShellcheckWarnings {
			break
		}
	}
	return warns
}
