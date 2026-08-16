package term

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
)

// OSC 52: set the *local* clipboard from wherever the shell is running
// (#140).
//
// This is the one clipboard mechanism that works unchanged over ssh.
// The shell writes an escape sequence, the terminal emulator at the
// human's end decodes it and sets their clipboard — so there is no X11
// forwarding, no reverse tunnel to pbcopy, no tmux buffer gymnastics,
// and nothing to install on the far side. The bytes ride the ssh
// channel gish is already using, which is why #98 split this out as
// "independent and nearly free".
//
// It lives in internal/term because that is the only package allowed to
// know about escape sequences.

// Clipboard selections, as OSC 52 spells them.
const (
	selectionClipboard = "c"
	selectionPrimary   = "p" // X11 middle-click; ignored elsewhere
)

// maxClipboardBytes caps the *encoded* payload.
//
// The limit is real and shared: tmux historically capped its buffer,
// and several terminals cap around 74–100KB. Past the cap the sequence
// is not truncated silently — a truncated base64 blob decodes to
// garbage, so the caller is told and nothing is emitted.
const maxClipboardBytes = 74 * 1024

// ClipboardWritable reports whether writing the clipboard can work
// here: a real terminal that is willing to render escapes. Same gate as
// every other escape-emitting surface, so NO_COLOR, TERM=dumb, pipes,
// and CI all get nothing.
func ClipboardWritable(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && IsTerminal(f)
}

// SetClipboard writes text to the terminal's clipboard selection.
//
// **Set only, never query.** OSC 52 also defines a read form, where the
// terminal replies with the clipboard's current contents. gish will not
// use it: a shell that can read your clipboard can exfiltrate whatever
// you last copied — a password, a recovery code — and that is exactly
// the capability terminals disable OSC 52 by default to prevent. There
// is no legitimate need for it here, so the read side is simply not
// implemented rather than implemented and discouraged.
func SetClipboard(w io.Writer, text string) error {
	return setSelection(w, selectionClipboard, text)
}

// SetPrimary writes the X11 primary selection (middle-click paste).
// Terminals that have no such concept ignore it.
func SetPrimary(w io.Writer, text string) error {
	return setSelection(w, selectionPrimary, text)
}

func setSelection(w io.Writer, selection, text string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	if len(encoded) > maxClipboardBytes {
		return fmt.Errorf("clipboard: %d bytes is over the ~%dKB terminals accept",
			len(text), maxClipboardBytes/1024)
	}
	_, err := io.WriteString(w, wrapPassthrough(fmt.Sprintf("\x1b]52;%s;%s\x07", selection, encoded)))
	return err
}

// wrapPassthrough re-frames a sequence so a multiplexer forwards it to
// the real terminal instead of eating it.
//
// tmux and screen intercept escape sequences they do not recognize, so
// an unwrapped OSC 52 inside them reaches nothing. Both offer a
// passthrough form, and they disagree about it:
//
//   - tmux wraps the whole thing in DCS tmux ... ST and requires every
//     inner ESC to be doubled.
//   - screen wraps in DCS ... ST and additionally cannot carry a long
//     payload in one chunk, which is a second reason the size cap above
//     is not merely advisory.
//
// Detection is by environment because there is nothing else to go on:
// $TMUX is set by tmux itself, and $STY by screen.
func wrapPassthrough(seq string) string {
	switch {
	case os.Getenv("TMUX") != "":
		// tmux: ESC doubled inside, DCS tmux ... ST outside. Requires
		// `set -g allow-passthrough on` in tmux 3.3+, which is why the
		// doctor line mentions it.
		return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	case strings.Contains(os.Getenv("TERM"), "screen") && os.Getenv("STY") != "":
		return "\x1bP" + seq + "\x1b\\"
	}
	return seq
}

// ClipboardTerminal names the terminal for `doctor`, and says what is
// known about its OSC 52 support. Several terminals ship it disabled by
// default — for the good reason described on SetClipboard — so "not
// working" is frequently a setting rather than a bug, and doctor should
// say which.
func ClipboardTerminal() (detail string, known bool) {
	if os.Getenv("TMUX") != "" {
		return "inside tmux: needs `set -g allow-passthrough on` (tmux 3.3+)", true
	}
	switch {
	case os.Getenv("KITTY_WINDOW_ID") != "":
		return "kitty: OSC 52 write enabled by default", true
	case os.Getenv("WEZTERM_PANE") != "":
		return "WezTerm: OSC 52 write enabled by default", true
	case os.Getenv("GHOSTTY_RESOURCES_DIR") != "":
		return "Ghostty: OSC 52 write enabled by default", true
	case os.Getenv("TERM_PROGRAM") == "iTerm.app":
		return "iTerm2: enable Preferences → General → Selection → " +
			"“Applications in terminal may access clipboard”", true
	case os.Getenv("TERM_PROGRAM") == "vscode":
		return "VS Code: OSC 52 write supported", true
	case os.Getenv("ALACRITTY_WINDOW_ID") != "":
		return "Alacritty: OSC 52 write enabled by default", true
	}
	return "emitted; this terminal may or may not act on it", false
}
