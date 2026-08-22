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
//	:%        the last ?search? word     :q  quote the whole result
//	:n-m      a range                   :x  quote, word by word
//	:n*       n through the last        :s/old/new/  substitute
//	:n-       n through the second last  :gs/old/new/ substitute everywhere
//	:-m       0 through m                :&  repeat the last substitution
//	:n-$      n through the last
//
// A word designator may only come first; everything after it is a
// modifier, and modifiers chain left to right — `!!:1:h:t` is word one,
// then its directory, then that directory's own last component.
//
// Three of those came with #692, because the event designators it added
// are what made them reachable, and each is measured: a range may leave
// its *start* out (`!!:-$` is the whole command) or end at `$`; `%` is
// the word a `!?string?` search matched, and the empty string when there
// has not been one; and the five characters `^$*%-` may stand with no `:`
// in front of them at all, which is what `!shopt-1` and `!!*` are.

// selection is what a designator produced: the text, and whether the
// line is to be printed rather than run (`:p`).
type selection struct {
	text      string
	printOnly bool
}

// applySelectors reads the designators following an event and applies
// them to command. runes starts at the first one — a ':', or one of the
// word designators bash lets stand with no ':' in front of it.
//
// A zero consumed count means there was nothing to read, which leaves
// the text to the caller; an error means there *was* something and it
// could not be read, which aborts the line the way bash does.
func applySelectors(command string, runes []rune, chars histChars) (selection, int, error) {
	sel := selection{text: command}
	consumed := 0
	wordsTaken := false
	// A word designator may follow the event with no `:` in front of it —
	// `!shopt-1`, `!!*`, `!-1$` — and bash's set for that is exactly the
	// characters that end an event's own text, which is why they need no
	// separator (measured: `!shopt-1` is the `shopt` line's words 0-1).
	if len(runes) > 0 && strings.ContainsRune("^$*%-", runes[0]) {
		text, n, found, err := wordDesignator(sel.text, runes, chars)
		switch {
		case err != nil:
			return selection{}, 0, errBadWord(string(runes[:n]))
		case found:
			sel.text, consumed, wordsTaken = text, n, true
		}
	}
	for consumed < len(runes) && runes[consumed] == ':' {
		i := consumed + 1
		if i >= len(runes) {
			// A trailing `:` is bash's error too: `!!:` fails rather
			// than expanding to the event and leaving the colon — and it
			// reports the modifier it did not find, which is nothing.
			return selection{}, 0, errBadModifier("")
		}
		if !wordsTaken {
			text, n, ok, err := wordDesignator(sel.text, runes[i:], chars)
			if err != nil {
				return selection{}, 0, errBadWord(string(runes[consumed : consumed+1+n]))
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
				err = errBadModifier(string(runes[i]))
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
func wordDesignator(command string, runes []rune, chars histChars) (string, int, bool, error) {
	fields := historyTokenize(command, chars.comment)
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
	case '%':
		// The word matched by the most recent `?string?` search, and the
		// empty string when there has not been one — measured: `!%` with
		// no search behind it expands to nothing rather than failing.
		return lastHistSearch.word, 1, true, nil
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
	// A range or a single word. The start may be left out — `-$` is
	// `0-$`, which is what makes `!!:-$` the whole command (measured) —
	// and the end may be a number, `$` for the last word, or left out.
	from, j := 0, 0
	switch {
	case isDigit(runes[0]):
		for j < len(runes) && isDigit(runes[j]) {
			j++
		}
		from = atoiRunes(runes[:j])
	case runes[0] != '-':
		return "", 0, false, nil
	}
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
	case j+1 < len(runes) && runes[j] == '-' && runes[j+1] == '$':
		text, err := join(from, len(fields)-1)
		return text, j + 2, true, err
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

// errBadWord is bash's answer to a word designator it cannot satisfy —
// `:9: bad word specifier` — with the designator spelled as it was
// written, leading `:` included when there was one.
func errBadWord(designator string) error {
	return fmt.Errorf("%s: bad word specifier", designator)
}

// errBadModifier is bash's answer to a `:` followed by something that is
// not a modifier at all, which names the character and not the event.
func errBadModifier(modifier string) error {
	return fmt.Errorf("%s: unrecognized history modifier", modifier)
}

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
	new = substReplacement(new, old)
	if global {
		return strings.ReplaceAll(s, old, new)
	}
	return strings.Replace(s, old, new, 1)
}

// substReplacement reads the replacement half of `:s/old/new/`, where an
// unquoted `&` stands for the text that matched — `!!:gs/foo/bar&/` turns
// `foo.c` into `barfoo.c` — and `\&` is a literal ampersand with the
// backslash dropped. Measured; the anchor for the parameter-expansion
// spelling of the same rule is #643.
func substReplacement(new, old string) string {
	if !strings.ContainsAny(new, "&\\") {
		return new
	}
	var b strings.Builder
	runes := []rune(new)
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == '\\' && i+1 < len(runes) && runes[i+1] == '&':
			b.WriteRune('&')
			i++
		case runes[i] == '&':
			b.WriteString(old)
		default:
			b.WriteRune(runes[i])
		}
	}
	return b.String()
}

// lastHistSearch remembers what the most recent `?string?` event matched,
// which is the only thing that can answer the `%` word designator.
var lastHistSearch struct{ word string }

// rememberSearchWord records the word of entry that contained query, for
// a later `%`. bash keeps the *word*, not the query, and it keeps the
// *last* word that contains it rather than the first — measured on
// `echo aXb cXd`, where `!?X?:%` is `cXd`.
func rememberSearchWord(entry, query string, chars histChars) {
	lastHistSearch.word = ""
	for _, f := range historyTokenize(entry, chars.comment) {
		if strings.Contains(f, query) {
			lastHistSearch.word = f
		}
	}
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

// historyTokenize splits a history entry into the words a designator
// selects from, the way readline's `history_tokenize` does (#709).
//
// This used to be `strings.Fields`, which is the shell's word splitting
// and not history's: readline breaks a shell metacharacter off into a
// word of its own, so `shopt a b c d 2>/dev/null` is seven words there
// and five here. The visible difference is a space — `!-2*` expands to
// `a b c d 2> /dev/null`, since the words rejoin with one — and the
// invisible one is worse, because the word *count* moves with it: `!!:$`
// on that entry is `/dev/null` in bash and `2>/dev/null` under the old
// rule, and every numbered designator after the operator was off by one.
// histexp.tests carries the case deliberately, with a comment saying that
// bash through 4.3 got it wrong.
//
// The rules are read off bash 5.3's lib/readline/histexpand.c rather than
// recalled, because none of them is guessable: a file descriptor sticks to
// the operator it precedes (`2>` is one word), a duplicating form takes its
// digits with it (`>&2`, `<&3-`), a process substitution is one word
// however deep it nests (`<(echo tmp)` — and so is `$(…)` and an extglob
// `+(…)`, which `strings.Fields` already got right by accident), and a
// bare `(` or `)` is a word on its own.
//
// comment is the history comment character — `#` unless $histchars moved
// it (#695) — at which the entry stops being tokenized at all.
func historyTokenize(s string, comment rune) []string {
	var words []string
	for i := 0; i < len(s); {
		for i < len(s) && isHistFieldDelim(s[i]) {
			i++
		}
		if i >= len(s) || rune(s[i]) == comment {
			return words
		}
		start := i
		i = historyTokenizeWord(s, start)
		// A delimiter the whitespace skip above did not eat becomes a
		// field of its own, with any adjacent delimiters. Unreachable with
		// readline's own delimiter set, since every member of it is
		// handled below; kept because the set is a variable there.
		if i == start {
			i++
			for i < len(s) && isHistWordDelim(s[i]) {
				i++
			}
		}
		words = append(words, s[start:i])
	}
	return words
}

// historyTokenizeWord returns the index one past the word beginning at
// ind. It works on bytes because readline does and because every
// character it is looking for is ASCII, so no slice it takes can land
// inside a rune.
func historyTokenizeWord(s string, ind int) int {
	// at is readline reading a NUL-terminated string: one past the end is
	// a byte that matches nothing, which several of the lookaheads below
	// rely on rather than bounds-checking.
	at := func(k int) byte {
		if k < 0 || k >= len(s) {
			return 0
		}
		return s[k]
	}
	i := ind
	var delimiter, delimopen byte
	nestdelim := 0

	if histMember(at(i), "()\n") {
		return i + 1
	}

	// A digit run is a file descriptor when a redirection operator follows
	// it and part of an ordinary word otherwise, which is what keeps `2>`
	// together and leaves `2fast` alone.
	digitsAreWord := false
	if isDigitByte(at(i)) {
		j := i
		for j < len(s) && isDigitByte(s[j]) {
			j++
		}
		if j >= len(s) {
			return j
		}
		i = j
		digitsAreWord = at(j) != '<' && at(j) != '>'
	}

	if !digitsAreWord && histMember(at(i), "<>;&|") {
		peek := at(i + 1)
		switch {
		case peek == at(i):
			// `<<-` and `<<<` take the third character; `<<`, `>>`, `;;`,
			// `&&` and `||` take two.
			if peek == '<' && (at(i+2) == '-' || at(i+2) == '<') {
				i++
			}
			return i + 2
		case peek == '&' && (at(i) == '>' || at(i) == '<'):
			// `>&2`, `<&3-`: the descriptor and a closing `-` belong to the
			// operator.
			j := i + 2
			for j < len(s) && isDigitByte(s[j]) {
				j++
			}
			if at(j) == '-' {
				j++
			}
			return j
		case (peek == '>' && at(i) == '&') || (peek == '|' && at(i) == '>'):
			return i + 2
		case peek == '(' && (at(i) == '>' || at(i) == '<'):
			// A process substitution opening the word: read on to its
			// matching paren rather than stopping at the operator.
			i += 2
			delimopen, delimiter, nestdelim = '(', ')', 1
		default:
			return i + 1
		}
	}

	if delimiter == 0 && histMember(at(i), histQuoteChars) {
		delimiter = at(i)
		i++
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c == '\\' && at(i+1) == '\n' {
			i++
			continue
		}
		// readline asks whether the *backslash* is one of the characters a
		// double quote protects, which it always is, so the only quote that
		// stops an escape here is a single one. Copied rather than
		// corrected: it is what decides where a word ends in bash.
		if c == '\\' && delimiter != '\'' {
			i++
			if i >= len(s) {
				break
			}
			continue
		}
		if nestdelim > 0 && c == delimopen {
			nestdelim++
			continue
		}
		if nestdelim > 0 && c == delimiter {
			nestdelim--
			if nestdelim == 0 {
				delimiter = 0
			}
			continue
		}
		if delimiter != 0 && c == delimiter {
			delimiter = 0
			continue
		}
		// Command and process substitutions and extended globs: everything
		// to the matching paren is one word.
		if nestdelim == 0 && delimiter == 0 && histMember(c, "<>$!@?+*") && at(i+1) == '(' {
			i++
			if at(i+1) == 0 {
				break
			}
			delimopen, delimiter, nestdelim = '(', ')', 1
			continue
		}
		if delimiter == 0 && isHistWordDelim(c) {
			break
		}
		if delimiter == 0 && histMember(c, histQuoteChars) {
			delimiter = c
		}
	}
	return i
}

// histQuoteChars is readline's HISTORY_QUOTE_CHARACTERS: the three that
// open a span the word delimiters do not end.
const histQuoteChars = "\"'`"

// isHistFieldDelim is readline's fielddelim: what separates one word from
// the next and is thrown away rather than kept.
func isHistFieldDelim(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }

// isHistWordDelim reports membership of history_word_delimiters, which
// ends a word without necessarily being thrown away.
func isHistWordDelim(c byte) bool { return histMember(c, histWordDelimiters) }

// histMember is readline's member(), which answers false for the NUL that
// stands for the end of the string.
func histMember(c byte, set string) bool {
	return c != 0 && strings.IndexByte(set, c) >= 0
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }
