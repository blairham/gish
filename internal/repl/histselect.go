package repl

import (
	"fmt"
	"path"
	"strings"
)

// Word designators and modifiers — the second half of history expansion
// (#96, #277).
//
// `!!` was implemented with `:N` and `:N-M` and nothing else, so the
// rest of the vocabulary silently passed through as text: `!!:$` ran a
// command with a literal `:$` on the end, and `!!:z` — which is not a
// modifier at all — did the same rather than failing. bash treats every
// one of these as part of the designator and refuses the line when it
// cannot read one, which is what makes a typo a diagnostic instead of a
// different command.
//
// The vocabulary here is bash's, measured rather than recalled:
//
//	:0        the command word          :h  head, drop the last /component
//	:n        the nth word              :t  tail, keep it
//	:^        the first argument        :r  root, drop the .suffix
//	:$        the last word             :e  extension, keep it
//	:*        every argument            :p  print rather than run
//	:n-m      a range                   :q  quote the whole result
//	:n*       n through the last        :x  quote, word by word
//	:n-       n through the second last  :s/old/new/  substitute
//	                                    :gs/old/new/ substitute everywhere
//	                                    :&  repeat the last substitution
//
// A word designator may only come first; everything after it is a
// modifier, and modifiers chain left to right — `!!:1:h:t` is word one,
// then its directory, then that directory's own last component.

// selection is what a designator produced: the text, and whether the
// line is to be printed rather than run (`:p`).
type selection struct {
	text      string
	printOnly bool
}

// applySelectors reads the `:`-introduced designators following an
// event and applies them to command. runes starts at the first ':'.
//
// A zero consumed count means there was nothing to read, which leaves
// the text to the caller; an error means there *was* something and it
// could not be read, which aborts the line the way bash does.
func applySelectors(command string, runes []rune, ev string) (selection, int, error) {
	sel := selection{text: command}
	consumed := 0
	wordsTaken := false
	for consumed < len(runes) && runes[consumed] == ':' {
		i := consumed + 1
		if i >= len(runes) {
			// A trailing `:` is bash's error too: `!!:` fails rather
			// than expanding to the event and leaving the colon.
			return selection{}, 0, errExpansionFailed(ev + string(runes[consumed:]))
		}
		if !wordsTaken {
			text, n, ok, err := wordDesignator(sel.text, runes[i:])
			if err != nil {
				return selection{}, 0, errExpansionFailed(ev + string(runes[consumed:consumed+1+n]))
			}
			if ok {
				sel.text, consumed, wordsTaken = text, i+n, true
				continue
			}
		}
		wordsTaken = true
		next, n, err := applyModifier(sel, runes[i:])
		if err != nil || n == 0 {
			if err == nil {
				err = errExpansionFailed(ev + string(runes[consumed:]))
			}
			return selection{}, 0, err
		}
		sel, consumed = next, i+n
	}
	return sel, consumed, nil
}

// wordDesignator selects words of the command. ok is false when runes
// does not start with one, which is how a leading modifier — `!!:h` —
// falls through to the modifiers.
//
// A word that is not there is an *error* rather than an empty string,
// which is bash's answer and the useful one: `!!:9` on a two-word
// command is a typo, and running the rest of the line without it would
// be a different command. The two exceptions are measured, not guessed:
// `:*` on a command with no arguments is empty, and `:$` on a one-word
// command is that word.
func wordDesignator(command string, runes []rune) (string, int, bool, error) {
	fields := strings.Fields(command)
	word := func(n int) (string, error) {
		if n < 0 || n >= len(fields) {
			return "", errRange
		}
		return fields[n], nil
	}
	join := func(from, to int) (string, error) {
		if from < 0 || from > to || from >= len(fields) || to >= len(fields) {
			return "", errRange
		}
		return strings.Join(fields[from:to+1], " "), nil
	}
	switch runes[0] {
	case '^':
		text, err := word(1)
		return text, 1, true, err
	case '$':
		text, err := word(len(fields) - 1)
		return text, 1, true, err
	case '*':
		// Every argument, and the empty string when there are none —
		// not an error, which is what makes `!!:*` safe to write for a
		// command that sometimes takes no arguments.
		if len(fields) < 2 {
			return "", 1, true, nil
		}
		text, err := join(1, len(fields)-1)
		return text, 1, true, err
	}
	if !isDigit(runes[0]) {
		return "", 0, false, nil
	}
	j := 0
	for j < len(runes) && isDigit(runes[j]) {
		j++
	}
	from := atoiRunes(runes[:j])
	switch {
	case j < len(runes) && runes[j] == '*':
		text, err := join(from, len(fields)-1)
		return text, j + 1, true, err
	case j+1 < len(runes) && runes[j] == '-' && isDigit(runes[j+1]):
		k := j + 1
		for k < len(runes) && isDigit(runes[k]) {
			k++
		}
		text, err := join(from, atoiRunes(runes[j+1:k]))
		return text, k, true, err
	case j < len(runes) && runes[j] == '-':
		// `n-` stops one short of the last word, which is the whole
		// difference between it and `n*` — and it still requires word n
		// to be there.
		if from >= len(fields) {
			return "", j + 1, true, errRange
		}
		text, err := join(from, len(fields)-2)
		if from > len(fields)-2 {
			text, err = "", nil // n is the last word: nothing between
		}
		return text, j + 1, true, err
	}
	text, err := word(from)
	return text, j, true, err
}

// errRange is a word designator naming a word the command does not
// have; the caller turns it into bash's message with the designator in
// it, which is the part a reader needs.
var errRange = fmt.Errorf("out of range")

// applyModifier applies one modifier. A zero count means the rune is
// not a modifier at all, which the caller turns into a failed
// expansion.
func applyModifier(sel selection, runes []rune) (selection, int, error) {
	switch runes[0] {
	case 'h':
		sel.text = pathHead(sel.text)
		return sel, 1, nil
	case 't':
		if i := strings.LastIndexByte(sel.text, '/'); i >= 0 {
			sel.text = sel.text[i+1:]
		}
		return sel, 1, nil
	case 'r':
		sel.text = strings.TrimSuffix(sel.text, path.Ext(lastComponent(sel.text)))
		return sel, 1, nil
	case 'e':
		sel.text = path.Ext(lastComponent(sel.text))
		return sel, 1, nil
	case 'p':
		// The one modifier that is not about the text: the line is
		// printed and not run, which is how `!!:p` exists to be a
		// preview rather than a rerun.
		sel.printOnly = true
		return sel, 1, nil
	case 'q':
		sel.text = singleQuote(sel.text)
		return sel, 1, nil
	case 'x':
		// Like q, but each word is quoted separately, so the result
		// still splits into the words it came from.
		fields := strings.Fields(sel.text)
		for i, f := range fields {
			fields[i] = singleQuote(f)
		}
		sel.text = strings.Join(fields, " ")
		return sel, 1, nil
	case 's', 'g', '&':
		return substituteModifier(sel, runes)
	}
	return sel, 0, nil
}

// substituteModifier handles `s/old/new/`, its `g` prefix, and `&`,
// which repeats whatever the last substitution was.
func substituteModifier(sel selection, runes []rune) (selection, int, error) {
	i := 0
	global := false
	if runes[i] == 'g' {
		global = true
		i++
		if i >= len(runes) {
			return sel, 0, nil
		}
	}
	if runes[i] == '&' {
		if lastHistSubst.old == "" {
			return sel, 0, nil
		}
		sel.text = replace(sel.text, lastHistSubst.old, lastHistSubst.new, global)
		return sel, i + 1, nil
	}
	if runes[i] != 's' {
		return sel, 0, nil
	}
	i++
	if i >= len(runes) {
		return sel, 0, nil
	}
	// The delimiter is whatever follows the s, which is usually / and
	// does not have to be.
	delim := runes[i]
	i++
	old, n, ok := untilDelim(runes[i:], delim)
	if !ok {
		return sel, 0, nil
	}
	i += n
	// A trailing delimiter is optional at the end of the line, which is
	// why the new text is read to the end when there is none.
	newText, n, _ := untilDelim(runes[i:], delim)
	i += n
	lastHistSubst.old, lastHistSubst.new = old, newText
	sel.text = replace(sel.text, old, newText, global)
	return sel, i, nil
}

// lastHistSubst remembers the last `s/old/new/` for `&` to repeat, the
// way bash's history does. One per session, like the history itself.
var lastHistSubst struct{ old, new string }

// untilDelim reads up to the next unescaped delimiter, returning what
// it read and how much it consumed including the delimiter.
func untilDelim(runes []rune, delim rune) (string, int, bool) {
	var b strings.Builder
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\\':
			if i+1 < len(runes) && runes[i+1] == delim {
				b.WriteRune(delim)
				i++
				continue
			}
			b.WriteRune(runes[i])
		case delim:
			return b.String(), i + 1, true
		default:
			b.WriteRune(runes[i])
		}
	}
	return b.String(), len(runes), true
}

func replace(s, old, new string, global bool) string {
	if old == "" {
		return s
	}
	if global {
		return strings.ReplaceAll(s, old, new)
	}
	return strings.Replace(s, old, new, 1)
}

// pathHead drops the last /component, and answers the string itself
// when there is none — `!!:h` on a command with no path in it leaves it
// alone rather than emptying it.
func pathHead(s string) string {
	i := strings.LastIndexByte(s, '/')
	switch {
	case i < 0:
		return s
	case i == 0:
		return "/"
	}
	return s[:i]
}

func lastComponent(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func errExpansionFailed(designator string) error {
	return fmt.Errorf("%s: history expansion failed", designator)
}
