package repl

import (
	"fmt"
	"io"
	"os"

	"mvdan.cc/sh/v3/interp"
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
	oscPromptStart  = "\x1b]133;A\x1b\\"
	oscPromptEnd    = "\x1b]133;B\x1b\\"
	oscOutputStart  = "\x1b]133;C\x1b\\"
	oscCommandDoneF = "\x1b]133;D;%d\x1b\\"
)

// semanticMarksOn reports whether to emit marks. On by default —
// they are inert in terminals that do not understand them — with an
// escape hatch for the rare terminal that renders unknown OSC badly.
func semanticMarksOn(runner *interp.Runner) bool {
	return shellVar(runner, "GISH_SEMANTIC_MARKS", "on") != "off"
}

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

// doctorSemanticMarks reports the terminal's block-navigation support
// for `doctor`. Terminals are identified by the variables they set;
// an unknown terminal is not a problem, just an unknown.
func doctorSemanticMarks() (detail string, known bool) {
	switch {
	case os.Getenv("KITTY_WINDOW_ID") != "":
		return "kitty: scroll-to-prompt and select-output work", true
	case os.Getenv("WEZTERM_PANE") != "":
		return "WezTerm: scroll-to-prompt and select-output work", true
	case os.Getenv("GHOSTTY_RESOURCES_DIR") != "":
		return "Ghostty: scroll-to-prompt and select-output work", true
	case os.Getenv("TERM_PROGRAM") == "iTerm.app":
		return "iTerm2: shell integration features work", true
	case os.Getenv("TERM_PROGRAM") == "vscode":
		return "VS Code: command decorations and navigation work", true
	}
	return "emitted; this terminal may or may not use them", false
}
