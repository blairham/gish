package editor

import (
	"strings"

	"github.com/blairham/gish/internal/term"
)

// `bind -x` (#159): a key sequence bound to a shell command.
//
// This is how fzf installs Ctrl-T and Ctrl-R, how atuin takes over
// history search, and how a great many rc files add one-key shortcuts.
// It is also the only hook in the surface that needs the *editor*
// rather than the loop, because the thing being bound is a keystroke.
//
// readline's contract with a bound command is READLINE_LINE and
// READLINE_POINT: the command reads them, may rewrite them, and the
// editor takes back whatever it left. The command runs with the
// terminal ceded, because the interesting ones are full-screen — fzf
// draws a picker, and a picker cannot share stdin with a line editor.

// KeyCommand runs a `bind -x` command. It receives the buffer and
// cursor, and returns their replacements; ok=false leaves both alone.
type KeyCommand func(command, line string, point int) (string, int, bool)

// BindKeyCommand binds a readline key sequence to a shell command.
// Reports whether the sequence was understood — an unparsable one is
// the caller's to report, since it came from the user's rc.
func (e *Editor) BindKeyCommand(seq, command string) bool {
	b, ok := parseKeySeq(seq)
	if !ok {
		return false
	}
	if e.keyCommands == nil {
		e.keyCommands = map[binding]string{}
	}
	e.keyCommands[b] = command
	return true
}

// UnbindKey removes a `bind -x` binding (readline's `bind -r`).
func (e *Editor) UnbindKey(seq string) bool {
	b, ok := parseKeySeq(seq)
	if !ok {
		return false
	}
	delete(e.keyCommands, b)
	return true
}

// BoundKeys returns the bound sequences, for `bind -X`.
func (e *Editor) BoundKeys() map[string]string {
	out := map[string]string{}
	for b, cmd := range e.keyCommands {
		out[keySeqString(b)] = cmd
	}
	return out
}

// runKeyCommand dispatches a bound key, if any. Reports whether the key
// was claimed.
func (e *Editor) runKeyCommand(ev term.KeyEvent) bool {
	cmd, ok := e.keyCommands[binding{key: ev.Key, r: ev.Rune, mod: ev.Mod}]
	if !ok || e.cfg.KeyCommand == nil {
		return false
	}
	e.handover(func(line string, point int) (string, int, bool) {
		return e.cfg.KeyCommand(cmd, line, point)
	})
	return true
}

// parseKeySeq parses a readline key sequence — `"\C-t"`, `"\et"`,
// `"\C-x\C-e"`, `"\e[A"` — into a binding.
//
// Only single-chord sequences are claimed. readline's multi-key
// sequences beyond the Ctrl-X prefix would need a trie in the
// dispatcher, and reporting "not understood" for those is better than
// binding the first key of one and swallowing the rest, which is how a
// half-understood sequence turns into a shell that eats keystrokes.
func parseKeySeq(seq string) (binding, bool) {
	s := strings.Trim(seq, `"'`)
	if s == "" {
		return binding{}, false
	}

	var mod term.Mod
	for {
		switch {
		case strings.HasPrefix(s, `\C-`):
			mod |= term.ModCtrl
			s = s[3:]
			continue
		case strings.HasPrefix(s, `\M-`), strings.HasPrefix(s, `\e`):
			mod |= term.ModAlt
			if strings.HasPrefix(s, `\M-`) {
				s = s[3:]
			} else {
				s = s[2:]
			}
			continue
		}
		break
	}

	// Named escapes for the keys that are not runes.
	switch s {
	case `\t`, "\t":
		return binding{key: term.KeyTab, mod: mod}, true
	case `\r`, "\r", `\n`, "\n":
		return binding{key: term.KeyEnter, mod: mod}, true
	case `\d`, "\x7f":
		return binding{key: term.KeyBackspace, mod: mod}, true
	}
	runes := []rune(s)
	if len(runes) != 1 {
		return binding{}, false
	}
	return binding{key: term.KeyRune, r: runes[0], mod: mod}, true
}

// keySeqString renders a binding back in readline's spelling, for the
// listing `bind -X` prints.
func keySeqString(b binding) string {
	var sb strings.Builder
	sb.WriteString(`"`)
	if b.mod&term.ModAlt != 0 {
		sb.WriteString(`\e`)
	}
	if b.mod&term.ModCtrl != 0 {
		sb.WriteString(`\C-`)
	}
	switch b.key {
	case term.KeyTab:
		sb.WriteString(`\t`)
	case term.KeyEnter:
		sb.WriteString(`\r`)
	case term.KeyBackspace:
		sb.WriteString(`\d`)
	default:
		sb.WriteRune(b.r)
	}
	sb.WriteString(`"`)
	return sb.String()
}
