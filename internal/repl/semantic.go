package repl

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"

	"github.com/blairham/gish/internal/history"
)

// OSC 133 semantic prompt marks (#99): the standard way a shell tells
// the terminal where a prompt starts, where the user's input begins,
// where a command's output begins, and what it exited with.
//
// This is the cheap half of blocks, and it is not a consolation prize:
// kitty, WezTerm, Ghostty, iTerm2, and VS Code already implement
// scroll-to-previous-prompt, select-command-output, and click-to-rerun
// on top of these marks. Emitting them means gish users get block
// navigation *today*, in the terminal they already run, without gish
// having to become a terminal.
//
// The sequences:
//
//	OSC 133 ; A ST   prompt starts
//	OSC 133 ; B ST   prompt ends, user input starts
//	OSC 133 ; C ST   command output starts
//	OSC 133 ; D ; N  command finished with status N
//
// Marks are zero-width, so the editor's ANSI-aware renderer treats
// them as it treats color: present in the byte stream, absent from
// every width calculation.

const (
	// The A mark declares click_events=1: the 2026 addition to the
	// protocol (kitty, and Ghostty since PR #10536) where the terminal
	// hands prompt interaction *back* to the shell. A click in the
	// prompt arrives as ordinary arrow keys, which the editor already
	// handles — so declaring support is the whole implementation, and
	// not declaring it is the only reason it would not work.
	oscPromptStart  = "\x1b]133;A;click_events=1\x1b\\"
	oscPromptEnd    = "\x1b]133;B\x1b\\"
	oscOutputStart  = "\x1b]133;C\x1b\\"
	oscCommandDoneF = "\x1b]133;D;%d\x1b\\"
	// OSC 7 reports the working directory as a file URL. It is what
	// makes a new tab or split open where you were, and every modern
	// terminal consumes it — it is also, unlike OSC 133, something the
	// shell must re-send on every change rather than once.
	oscCwdF = "\x1b]7;file://%s%s\x1b\\"
	// OSC 1337 SetUserVar is iTerm2's, adopted by WezTerm: arbitrary
	// key/value pairs a terminal can surface in a status line or use in
	// its own rules.
	oscUserVarF = "\x1b]1337;SetUserVar=%s=%s\x1b\\"
)

// termFeatures is what this session emits. Per-feature rather than
// all-or-nothing (#165): the marks are universally safe, OSC 7 is
// nearly so, and SetUserVar writes the command line into a place the
// terminal may display — which is a reasonable thing to want off
// without also losing block navigation.
type termFeatures struct {
	marks    bool // OSC 133 A/B/C/D
	cwd      bool // OSC 7
	userVars bool // OSC 1337 SetUserVar
}

// semanticFeatures parses GISH_SEMANTIC_MARKS: `on` (everything, the
// default), `off` (nothing), or a comma-separated subset of
// marks,cwd,uservars.
func semanticFeatures(runner *interp.Runner) termFeatures {
	setting := strings.ToLower(strings.TrimSpace(shellVar(runner, "GISH_SEMANTIC_MARKS", "on")))
	switch setting {
	case "", "on", "all":
		// SetUserVar is off in the default set on purpose: it is the
		// one that hands the command line to the terminal, and the
		// terminals that display it do so by default. Opt in.
		return termFeatures{marks: true, cwd: true}
	case "off", "none":
		return termFeatures{}
	}
	var f termFeatures
	for _, part := range strings.Split(setting, ",") {
		switch strings.TrimSpace(part) {
		case "marks":
			f.marks = true
		case "cwd":
			f.cwd = true
		case "uservars", "uservar":
			f.userVars = true
		}
	}
	return f
}

// semanticMarksOn is the marks half, which most callers want.
func semanticMarksOn(runner *interp.Runner) bool { return semanticFeatures(runner).marks }

// markPrompt wraps a rendered prompt in the A and B marks. Wrapping
// the prompt string (rather than writing marks around the editor's
// render) is deliberate: the renderer already treats escapes as
// zero-width and atomic, so the marks survive redraws, wrapping, and
// multi-line prompts without special cases.
func markPrompt(prompt string, on bool) string {
	if !on {
		return prompt
	}
	return oscPromptStart + prompt + oscPromptEnd
}

// markOutputStart tells the terminal the next bytes are command
// output, not prompt or input.
func markOutputStart(w io.Writer, on bool) {
	if on {
		io.WriteString(w, oscOutputStart) //nolint:errcheck // terminal write
	}
}

// markCommandDone closes the block with the exit status, which is what
// lets a terminal color a failed command's marker.
func markCommandDone(w io.Writer, on bool, exitCode int) {
	if on {
		fmt.Fprintf(w, oscCommandDoneF, exitCode)
	}
}

// markCwd reports the working directory (OSC 7), so a new tab or split
// opens where the user is.
//
// The path is percent-encoded as a file URL: a directory named with a
// space or a '#' is ordinary, and an unencoded one produces a URL the
// terminal parses into somewhere else entirely.
func markCwd(w io.Writer, on bool, dir string) {
	if !on || dir == "" {
		return
	}
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	fmt.Fprintf(w, oscCwdF, host, encodePath(dir))
}

// encodePath percent-encodes a filesystem path for a file URL, leaving
// the separators alone.
func encodePath(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		switch {
		case r == '/' || r == '-' || r == '_' || r == '.' || r == '~',
			r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			for _, c := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
	}
	return b.String()
}

// markUserVars publishes the command and its duration (OSC 1337).
//
// The command line goes through the same secret rules as history (#10):
// a terminal may put this in a status bar, a window title, or its own
// logs, and "the shell told the terminal" is not a place a token should
// reach. A command that history would refuse to record is published as
// its first word only.
func markUserVars(w io.Writer, on bool, command string, duration time.Duration) {
	if !on {
		return
	}
	if history.SecretReason(command) != "" {
		command = firstWord(command)
	}
	fmt.Fprintf(w, oscUserVarF, "gish_command", base64.StdEncoding.EncodeToString([]byte(command)))
	fmt.Fprintf(w, oscUserVarF, "gish_duration_ms",
		base64.StdEncoding.EncodeToString([]byte(strconv.FormatInt(duration.Milliseconds(), 10))))
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t"); i > 0 {
		return s[:i]
	}
	return s
}

// doctorSemanticMarks reports the terminal's block-navigation support
// for `doctor`. Terminals are identified by the variables they set;
// an unknown terminal is not a problem, just an unknown.
func doctorSemanticMarks() (detail string, known bool) {
	t := detectTerminal()
	if t.name == "" {
		return "emitted; this terminal may or may not use them", false
	}
	return t.name + ": " + t.supports, t.known
}

// terminalInfo is what a terminal actually does with what gish emits.
//
// The per-affordance detail matters more than the name. docs/blocks.md
// used to say users "get block navigation today" in a list of terminals
// that included Ghostty and Alacritty; checking each one against its own
// documentation found that Ghostty has no output-retrieval API at all
// (its author is explicitly wary of escape-sequence-driven control) and
// Alacritty implements no OSC 133 whatsoever. Scroll-to-prompt and
// select-output are different features, and a shell that claims both
// where only one exists is the kind of claim the launch playbook exists
// to prevent.
type terminalInfo struct {
	name     string
	supports string
	known    bool
}

func detectTerminal() terminalInfo {
	switch {
	case os.Getenv("KITTY_WINDOW_ID") != "":
		return terminalInfo{"kitty", "scroll-to-prompt, select-output, click-to-move-cursor, OSC 7", true}
	case os.Getenv("WEZTERM_PANE") != "":
		return terminalInfo{"WezTerm", "scroll-to-prompt, select-output, SetUserVar, OSC 7", true}
	case os.Getenv("GHOSTTY_RESOURCES_DIR") != "" || os.Getenv("TERM_PROGRAM") == "ghostty":
		return terminalInfo{"Ghostty", "scroll-to-prompt, click-to-move-cursor, OSC 7 — no output-retrieval API, so select-output is the terminal's own selection", true}
	case os.Getenv("TERM_PROGRAM") == "iTerm.app":
		return terminalInfo{"iTerm2", "shell integration: marks, SetUserVar, OSC 7", true}
	case os.Getenv("TERM_PROGRAM") == "vscode":
		return terminalInfo{"VS Code", "command decorations, navigation, OSC 7", true}
	case os.Getenv("ALACRITTY_WINDOW_ID") != "" || os.Getenv("TERM") == "alacritty":
		return terminalInfo{"Alacritty", "no OSC 133 support — the marks are inert here, and that is harmless", true}
	case os.Getenv("TERM_PROGRAM") == "Apple_Terminal":
		return terminalInfo{"Terminal.app", "OSC 7 only — no OSC 133 support", true}
	}
	return terminalInfo{}
}
