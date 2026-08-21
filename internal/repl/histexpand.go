package repl

import (
	"fmt"
	"strings"
)

// History expansion (#96): the bash/zsh event designators whose
// absence generates "how do I get !! back" threads. Interactive-only,
// applied to the raw line before parsing; the expanded line is echoed
// (bash behavior) so the user sees what actually runs.
//
// Supported: !!, !$, !^, !:N, !:N-M, !prefix, and ^old^new. Expansion
// is conservative: it never fires inside single quotes, and a `!`
// followed by space, =, or ( is left alone — the same lexical outs
// bash gives scripts that use ! for negation.

// expandHistory rewrites the line against the last matching history
// entries. changed=false means the line passes through untouched; a
// non-nil error aborts the line (bash prints "event not found").
func expandHistory(line string, last func(prefix string, n int) (string, bool)) (string, bool, error) {
	expanded, changed, _, err := expandHistoryLine(line, last)
	return expanded, changed, err
}

// expandHistoryLine is [expandHistory] with `:p` reported: the modifier
// asks for the expansion to be *shown* rather than run, which only the
// interactive caller can honor (#96).
func expandHistoryLine(line string, last func(prefix string, n int) (string, bool)) (string, bool, bool, error) {
	// ^old^new: whole-line substitution on the previous command.
	if strings.HasPrefix(line, "^") {
		expanded, changed, err := expandCaret(line, last)
		return expanded, changed, false, err
	}
	if !strings.Contains(line, "!") {
		return line, false, false, nil
	}

	var b strings.Builder
	changed, printOnly := false, false
	inSingle, inDouble := false, false
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '\\' && !inSingle && i+1 < len(runes):
			b.WriteRune(r)
			i++
			b.WriteRune(runes[i])
			continue
		case r == '!' && !inSingle:
			sel, consumed, err := expandEvent(runes[i:], last)
			if err != nil {
				return "", false, false, err
			}
			if consumed > 0 {
				b.WriteString(sel.text)
				printOnly = printOnly || sel.printOnly
				i += consumed - 1
				changed = true
				continue
			}
		}
		b.WriteRune(r)
	}
	if !changed {
		// Return the line itself, not the rune-rebuilt copy: rebuilding
		// converts invalid UTF-8 bytes to U+FFFD, so a pasted line in a
		// legacy encoding with a stray `!` would be silently mangled by
		// a pass that expanded nothing. Found by FuzzExpandHistory.
		return line, false, false, nil
	}
	return b.String(), true, printOnly, nil
}

// expandEvent handles one `!` designator starting at runes[0]. A zero
// consumed count means "not an event, leave the ! alone".
func expandEvent(runes []rune, last func(string, int) (string, bool)) (selection, int, error) {
	none := selection{}
	if len(runes) < 2 {
		return none, 0, nil
	}
	prev, ok := last("", 0)
	next := runes[1]
	// withSelectors reads the designators an event may be followed by —
	// `!!:$:h`, `!vi:p` — which apply to every event form and not only
	// to `!!` (#277).
	withSelectors := func(text string, consumed int, ev string) (selection, int, error) {
		sel, n, err := applySelectors(text, runes[consumed:], ev)
		if err != nil {
			return none, 0, err
		}
		if n == 0 {
			return selection{text: text}, consumed, nil
		}
		return sel, consumed + n, nil
	}
	switch {
	case next == ' ' || next == '\t' || next == '=' || next == '(':
		return none, 0, nil // bash's lexical outs: negation, != , !(
	case next == '!':
		if !ok {
			return none, 0, errNoEvent("!!")
		}
		return withSelectors(prev, 2, "!!")
	case next == '$':
		if !ok {
			return none, 0, errNoEvent("!$")
		}
		return withSelectors(lastArgOf(prev), 2, "!$")
	case next == '^':
		if !ok {
			return none, 0, errNoEvent("!^")
		}
		fields := strings.Fields(prev)
		if len(fields) < 2 {
			return none, 0, errNoEvent("!^")
		}
		return withSelectors(fields[1], 2, "!^")
	case next == ':':
		if !ok {
			return none, 0, errNoEvent("!:")
		}
		return withSelectors(prev, 1, "!")
	case isEventWordRune(next):
		// !prefix — most recent command starting with prefix.
		j := 1
		for j < len(runes) && isEventWordRune(runes[j]) {
			j++
		}
		prefix := string(runes[1:j])
		match, found := last(prefix, 0)
		if !found {
			return none, 0, errNoEvent("!" + prefix)
		}
		return withSelectors(match, j, "!"+prefix)
	}
	return none, 0, nil
}

// expandCaret is ^old^new: substitute in the previous command.
func expandCaret(line string, last func(string, int) (string, bool)) (string, bool, error) {
	parts := strings.SplitN(line, "^", 4)
	// parts[0] is empty; need at least old and new.
	if len(parts) < 3 || parts[1] == "" {
		return line, false, nil
	}
	prev, ok := last("", 0)
	if !ok {
		return "", false, errNoEvent("^" + parts[1] + "^")
	}
	if !strings.Contains(prev, parts[1]) {
		return "", false, fmt.Errorf("substitution failed: %q not in %q", parts[1], prev)
	}
	return strings.Replace(prev, parts[1], parts[2], 1), true, nil
}

// lastArgOf is the final whitespace-delimited word of a command line.
func lastArgOf(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func isEventWordRune(r rune) bool {
	return r == '-' || r == '_' || r == '.' || r == '/' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func atoiRunes(rs []rune) int {
	n := 0
	for _, r := range rs {
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			// A word index this large is only ever a typo, but letting
			// the arithmetic overflow turned it into a negative slice
			// index: `!:` plus twenty digits panicked the shell. Clamp
			// so the range check answers "event not found" instead.
			// Found by FuzzExpandHistory.
			return 1 << 30
		}
	}
	return n
}

func errNoEvent(designator string) error {
	return fmt.Errorf("%s: event not found", designator)
}
