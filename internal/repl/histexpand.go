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
	// ^old^new: whole-line substitution on the previous command.
	if strings.HasPrefix(line, "^") {
		return expandCaret(line, last)
	}
	if !strings.Contains(line, "!") {
		return line, false, nil
	}

	var b strings.Builder
	changed := false
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
			replacement, consumed, err := expandEvent(runes[i:], last)
			if err != nil {
				return "", false, err
			}
			if consumed > 0 {
				b.WriteString(replacement)
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
		return line, false, nil
	}
	return b.String(), true, nil
}

// expandEvent handles one `!` designator starting at runes[0]. A zero
// consumed count means "not an event, leave the ! alone".
func expandEvent(runes []rune, last func(string, int) (string, bool)) (string, int, error) {
	if len(runes) < 2 {
		return "", 0, nil
	}
	prev, ok := last("", 0)
	next := runes[1]
	switch {
	case next == ' ' || next == '\t' || next == '=' || next == '(':
		return "", 0, nil // bash's lexical outs: negation, != , !(
	case next == '!':
		if !ok {
			return "", 0, errNoEvent("!!")
		}
		if rest, n, err := applyWordDesignator(prev, runes[2:]); err == nil && n > 0 {
			return rest, 2 + n, nil
		}
		return prev, 2, nil
	case next == '$':
		if !ok {
			return "", 0, errNoEvent("!$")
		}
		return lastArgOf(prev), 2, nil
	case next == '^':
		if !ok {
			return "", 0, errNoEvent("!^")
		}
		fields := strings.Fields(prev)
		if len(fields) < 2 {
			return "", 0, errNoEvent("!^")
		}
		return fields[1], 2, nil
	case next == ':':
		if !ok {
			return "", 0, errNoEvent("!:")
		}
		rest, n, err := applyWordDesignator(prev, runes[1:])
		if err != nil {
			return "", 0, err
		}
		if n == 0 {
			return "", 0, nil
		}
		return rest, 1 + n, nil
	case isEventWordRune(next):
		// !prefix — most recent command starting with prefix.
		j := 1
		for j < len(runes) && isEventWordRune(runes[j]) {
			j++
		}
		prefix := string(runes[1:j])
		match, found := last(prefix, 0)
		if !found {
			return "", 0, errNoEvent("!" + prefix)
		}
		return match, j, nil
	}
	return "", 0, nil
}

// applyWordDesignator handles :N and :N-M against a command's words.
// runes starts at the ':'.
func applyWordDesignator(command string, runes []rune) (string, int, error) {
	if len(runes) < 2 || runes[0] != ':' {
		return "", 0, nil
	}
	j := 1
	for j < len(runes) && (runes[j] >= '0' && runes[j] <= '9') {
		j++
	}
	if j == 1 {
		return "", 0, nil
	}
	from := atoiRunes(runes[1:j])
	to := from
	consumed := j
	if j+1 < len(runes) && runes[j] == '-' && runes[j+1] >= '0' && runes[j+1] <= '9' {
		k := j + 1
		for k < len(runes) && (runes[k] >= '0' && runes[k] <= '9') {
			k++
		}
		to = atoiRunes(runes[j+1 : k])
		consumed = k
	}
	fields := strings.Fields(command)
	if from >= len(fields) || to >= len(fields) || from > to {
		return "", 0, errNoEvent(fmt.Sprintf("!:%d", from))
	}
	return strings.Join(fields[from:to+1], " "), consumed, nil
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
