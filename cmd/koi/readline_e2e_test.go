//go:build unix

package main

import (
	"strings"
	"testing"
)

// The readline keymap, driven by the bytes a terminal actually sends
// (#118).
//
// internal/editor's own tests for these bindings are good, and they
// cover a gap this one cannot: they feed `term.Event` values straight to
// the editor. That proves the *command* works. It cannot prove the
// binding works, because the step it skips is the one most likely to be
// broken — the decoder turning `\x1bu` into alt('u'). A keymap entry
// nothing decodes to is dead for the human at the terminal while every
// unit test stays green.
//
// So these send raw bytes into a real pty and assert on what the shell
// *ran*, which is the only evidence that survives both layers. One case
// per mechanism rather than per binding: the word operators share a
// decode path, so Alt-u standing in for Alt-l and Alt-c is honest, while
// the numeric argument and the two-key search are separate mechanisms
// and get their own.
func TestReadlineBindingsDecodeFromRealBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("pty e2e skipped in -short")
	}

	tests := []struct {
		name string
		// keys is typed literally, escapes included, then Enter.
		keys string
		// want is what the resulting command line printed — the edit is
		// asserted through its effect, never through the echo, which
		// would match the keystrokes rather than the result.
		want string
	}{
		{
			// Esc-f then Esc-u: both arrive as two-byte sequences, and
			// both have to survive the decoder as Alt rather than as a
			// literal Escape followed by a letter.
			name: "Alt-u upcases the word after the cursor",
			keys: "echo hello\x01\x1bf\x1bu",
			want: "HELLO",
		},
		{
			// The structural one (#116): Esc-3 accumulates an argument
			// that the *next* command consumes. If the digit is decoded
			// as a plain Alt-3 with nowhere to go, this inserts one z.
			name: "a numeric argument repeats the next command",
			keys: "echo \x1b3z",
			want: "zzz",
		},
		{
			// Ctrl-] claims the following key before any binding sees it,
			// so this proves the decoder hands over the *next* byte
			// rather than dispatching it.
			name: "Ctrl-] searches forward for a character",
			keys: "echo one two\x01\x1dtX",
			want: "one Xtwo",
		},
		{
			// Ctrl-V is the other next-key-claiming binding, and the one
			// that has to pass a control byte through untouched.
			name: "Ctrl-V inserts a literal tab",
			keys: "echo \"a\x16\tb\"",
			want: "a\tb",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := startPTY(t, ptyOptions{})
			s.waitForPrompt()

			// Cleared before the send and not touched afterwards: the
			// harness rule that keeps the D mark unambiguously this
			// command's.
			s.buf.Reset()
			s.send(tc.keys + "\r")
			s.waitFor(commandDone)

			// Only what the command *printed*, never the whole buffer.
			//
			// The editor redraws the line after every edit, so `echo
			// HELLO` is on screen before the command runs and a
			// buffer-wide match is satisfied by the redraw. Verified by
			// mutation: with the command changed to `true`, which prints
			// nothing at all, the buffer-wide version of this assertion
			// still passed. It was a redraw detector, not a binding test.
			got := commandOutput(s.plain())
			if !strings.Contains(got, tc.want) {
				t.Errorf("command printed %q, want it to contain %q — the binding decoded to something else:\n%s",
					got, tc.want, s.plain())
			}
		})
	}
}

// commandOutput returns what the shell printed between the OSC 133;C
// mark (output begins) and the D mark (command finished).
//
// Those marks exist precisely because "what did this command print" is
// otherwise unanswerable from a terminal transcript: the echo of the
// typed line, the prompt, and the output are one stream of bytes. The
// escape sequences survive ansiRe stripping as their payload, so the
// bracketing is done on the stripped text the assertions read.
func commandOutput(plain string) string {
	start := strings.Index(plain, "]133;C")
	if start < 0 {
		return ""
	}
	rest := plain[start+len("]133;C"):]
	if end := strings.Index(rest, "]133;D"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimLeft(rest, "\\\r\n")
}
