package repl

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// History expansion (#96): the bash/zsh event designators whose
// absence generates "how do I get !! back" threads. Applied to the raw
// line before parsing — interactively, over a script (#559) and over the
// shell's standard input (#694) — and the expanded line is echoed (bash
// behavior) so the user sees what actually runs.
//
// The event designators are bash's whole set: `!!`, `!n`, `!-n`,
// `!prefix`, `!?string?`, the word designators that may stand with no
// event of their own (`!$`, `!^`, `!*`, `!%`), and `^old^new`. #692 added
// the three that name an entry by number or by search, which were
// everything the suite had left; what a designator may be followed by
// lives in histselect.go.
//
// Expansion is conservative: it never fires inside single quotes, and a
// `!` followed by space, =, or ( is left alone — the same lexical outs
// bash gives scripts that use ! for negation.
//
// Three more outs matter once this runs over a *script* (#559), where
// the shapes they protect are ordinary shell rather than typing
// mistakes, and each is measured against bash rather than reasoned
// about:
//
//   - `[!A-Z])` is a negated bracket expression, not the event `!A-Z`,
//     so a `!` right after a `[` with a `]` still to come is left alone;
//   - `${!name}` is indirect expansion, so a `!` right after a `${`
//     with a `}` still to come is left alone;
//   - the history comment character — the third of histchars —
//     ends expansion for the rest of the line when it stands at the
//     start or after a word delimiter, which is what makes
//     `echo ok # !1200` an ordinary comment. It is positional and not a
//     quoting question: `echo x#y !!` and `echo "#" !!` both still
//     expand, while `true;# !!` does not.
//
// All three are bash's own, and the last two are `#SHELL` cases in
// readline's history library for exactly this reason.

// histSource is what the expander needs of the history list. Every event
// designator is one of these four questions, and they are separated
// because the list answers them differently: a prefix or a substring is a
// scan, a number is an index into the numbering `history` prints, and
// "n back" is the previous command counted from the end (#692).
type histSource struct {
	// prefix is the newest entry starting with prefix, skipping n
	// matches. The empty prefix with n=0 is the previous command, which
	// is what `!!` and `^old^new` take.
	prefix func(prefix string, n int) (string, bool)
	// search is the newest entry *containing* s. Nil when the caller has
	// no way to answer it, which makes `!?s?` an event not found rather
	// than a wrong answer.
	search func(s string) (string, bool)
	// numbered is the entry the `history` listing prints as n. Nil for
	// the same reason.
	numbered func(n int) (string, bool)
}

// back is the nth entry back, 1 being the previous command — `!-1`.
func (src histSource) back(n int) (string, bool) {
	if n < 1 || src.prefix == nil {
		return "", false
	}
	return src.prefix("", n-1)
}

// expandHistory rewrites the line against the last matching history
// entries. changed=false means the line passes through untouched; a
// non-nil error aborts the line (bash prints "event not found").
func expandHistory(line string, src histSource, chars histChars) (string, bool, error) {
	expanded, changed, _, err := expandHistoryLine(line, src, chars)
	return expanded, changed, err
}

// expandHistoryLine is [expandHistory] with `:p` reported: the modifier
// asks for the expansion to be *shown* rather than run, which only the
// interactive caller can honor (#96).
func expandHistoryLine(line string, src histSource, chars histChars) (string, bool, bool, error) {
	if chars.off() {
		// $histchars named no expansion character, so nothing in the
		// line can be one (#695).
		return line, false, false, nil
	}
	// ^old^new: whole-line substitution on the previous command. The
	// substitution character at the very start of a line is *always* this
	// form in bash, whatever follows it — `^X` with no closing character
	// and `^ X` are both attempted substitutions rather than commands
	// (measured) — so this either answers or fails and never falls
	// through to the scan below.
	if r, size := utf8.DecodeRuneInString(line); r == chars.subst {
		expanded, changed, err := expandCaret(line, line[size:], src, chars.subst)
		return expanded, changed, false, err
	}
	if !strings.ContainsRune(line, chars.expand) {
		return line, false, false, nil
	}

	var b strings.Builder
	changed, printOnly := false, false
	inSingle, inDouble := false, false
	// A command substitution opens a fresh quoting context, which is how
	// bash reads `echo "$(echo '!' )"` as an ordinary `!` while reading
	// the `!` in `echo "'!'"` as an event: in the first the `'` really is
	// a quote and in the second it is text inside the outer double
	// quotes. Measured both ways round.
	var quotes [][2]bool
	runes := []rune(line)
scan:
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '$' && !inSingle && i+1 < len(runes) && runes[i+1] == '(':
			quotes = append(quotes, [2]bool{inSingle, inDouble})
			inSingle, inDouble = false, false
			b.WriteRune(r)
			i++
			b.WriteRune(runes[i])
			continue
		case r == ')' && !inSingle && len(quotes) > 0:
			inSingle, inDouble = quotes[len(quotes)-1][0], quotes[len(quotes)-1][1]
			quotes = quotes[:len(quotes)-1]
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '\\' && !inSingle && i+1 < len(runes):
			b.WriteRune(r)
			i++
			b.WriteRune(runes[i])
			continue
		case r == chars.comment && !inSingle && !inDouble && atWordStart(runes, i):
			// The rest of the line is a comment as far as expansion is
			// concerned: bash copies it over and stops looking.
			b.WriteString(string(runes[i:]))
			break scan
		case r == chars.expand && (inSingle || (inDouble && chars.quotesInhibit)):
			// A single-quoted span is never scanned, and under posix a
			// double-quoted one is not either.
		case r == chars.expand && notAnEvent(runes, i):
		case r == chars.expand:
			// The quote a span is inside ends an event's text, which is
			// readline's delimiting_quote — so `echo "!"` leaves the `!`
			// alone while `echo "'!'"` is an event bash cannot find.
			quote := rune(0)
			if inDouble {
				quote = '"'
			}
			sel, consumed, err := expandEvent(runes[i:], src, chars, quote)
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
//
// The event forms are bash's whole set, and which of them wins where two
// could read the same text was measured rather than reasoned about
// (#692):
//
//   - the word designators `^ $ *` come *before* the doubled expansion
//     character, because with `histchars='^!#'` bash reads `^^` as the
//     expansion character plus the designator `^` — the first argument of
//     the previous command — rather than as "the previous command";
//   - `-` followed by a digit is a *relative event*, `!-1` being the
//     previous command, so it is read before the word designator `-n`
//     which is the range `0-n`;
//   - a bare digit run is an absolute event: `!2` is the entry the
//     `history` listing prints as 2, and the run stops at the first
//     non-digit, so `!2abc` is entry 2 with `abc` after it.
func expandEvent(runes []rune, src histSource, chars histChars, quote rune) (selection, int, error) {
	none := selection{}
	if len(runes) < 2 {
		return none, 0, nil
	}
	prev, ok := src.prefix("", 0)
	next := runes[1]
	// withSelectors reads the designators an event may be followed by —
	// `!!:$:h`, `!vi:p` — which apply to every event form and not only
	// to `!!` (#277).
	withSelectors := func(text string, consumed int) (selection, int, error) {
		sel, n, err := applySelectors(text, runes[consumed:], chars)
		if err != nil {
			return none, 0, err
		}
		if n == 0 {
			return selection{text: text}, consumed, nil
		}
		return sel, consumed + n, nil
	}
	// designator is a word designator standing where an event would, with
	// the previous command as its implicit event: `!$`, `!^`, `!*`.
	designator := func() (selection, int, error) {
		if !ok {
			return none, 0, errNoEvent(string(chars.expand) + string(next))
		}
		text, n, found, err := wordDesignator(prev, runes[1:], chars)
		switch {
		case err != nil:
			return none, 0, errBadWord(string(runes[1 : 1+n]))
		case !found:
			return none, 0, nil
		}
		return withSelectors(text, 1+n)
	}
	switch {
	case next == ' ' || next == '\t' || next == '=' || next == '(':
		return none, 0, nil // bash's lexical outs: negation, != , !(
	case next == '^' || next == '$' || next == '*' || next == '%':
		return designator()
	case next == chars.expand:
		if !ok {
			return none, 0, errNoEvent(string(chars.expand) + string(chars.expand))
		}
		return withSelectors(prev, 2)
	case next == '?':
		// !?string? — the most recent entry *containing* string. The
		// closing `?` is optional at the end of the line, which is why
		// the scan can run out rather than failing.
		j := 2
		for j < len(runes) && runes[j] != '?' {
			j++
		}
		query := string(runes[2:j])
		ev := string(chars.expand) + "?" + query
		if j < len(runes) {
			j++ // the closing `?` is part of the designator
			ev += "?"
		}
		if src.search == nil {
			return none, 0, errNoEvent(ev)
		}
		match, found := src.search(query)
		if !found {
			return none, 0, errNoEvent(ev)
		}
		rememberSearchWord(match, query, chars)
		return withSelectors(match, j)
	case next == ':':
		if !ok {
			return none, 0, errNoEvent(string(chars.expand) + ":")
		}
		return withSelectors(prev, 1)
	case next == '-' && len(runes) > 2 && isDigit(runes[2]):
		// !-n — n entries back, `!-1` being the previous command.
		j := 2
		for j < len(runes) && isDigit(runes[j]) {
			j++
		}
		ev := string(chars.expand) + string(runes[1:j])
		match, found := src.back(atoiRunes(runes[2:j]))
		if !found {
			return none, 0, errNoEvent(ev)
		}
		return withSelectors(match, j)
	case isDigit(next):
		// !n — the entry the `history` listing prints as n.
		j := 1
		for j < len(runes) && isDigit(runes[j]) {
			j++
		}
		ev := string(chars.expand) + string(runes[1:j])
		if src.numbered == nil {
			return none, 0, errNoEvent(ev)
		}
		match, found := src.numbered(atoiRunes(runes[1:j]))
		if !found {
			return none, 0, errNoEvent(ev)
		}
		return withSelectors(match, j)
	case !isEventDelimiter(next) && next != quote:
		// !prefix — most recent command starting with prefix.
		j := 1
		for j < len(runes) && !isEventDelimiter(runes[j]) && runes[j] != quote {
			j++
		}
		prefix := string(runes[1:j])
		match, found := src.prefix(prefix, 0)
		if !found {
			return none, 0, errNoEvent(string(chars.expand) + prefix)
		}
		return withSelectors(match, j)
	}
	return none, 0, nil
}

// expandCaret is ^old^new: substitute in the previous command. rest is
// line with the leading substitution character already removed, and subst
// is that character, which $histchars can move (#695).
//
// bash reports this form as the `:s` modifier it is equivalent to, naming
// the line as written — `:s^x^y: substitution failed` — and its three
// shapes were measured rather than derived from the documented one: the
// closing character is optional, an *empty* old repeats the last
// substitution (so a bare `^` is "no previous substitution"), and
// anything after the closing character is appended to the result.
func expandCaret(line, rest string, src histSource, subst rune) (string, bool, error) {
	parts := strings.SplitN(rest, string(subst), 3)
	old, newText, tail := parts[0], "", ""
	// as is the designator bash names in a diagnostic: what the line
	// spent on the substitution, with the appended tail left out — `^a^b^c`
	// is reported as `:s^a^b^`.
	as := ":s" + string(subst) + old
	if len(parts) > 1 {
		newText = parts[1]
		as += string(subst) + newText
	}
	if len(parts) > 2 {
		tail = parts[2]
		as += string(subst)
	}
	if old == "" {
		// `^^` is `:s` with nothing to substitute, which repeats whatever
		// the last substitution was — the same memory `:&` reads.
		if lastHistSubst.old == "" {
			return "", false, fmt.Errorf("%s: no previous substitution", as)
		}
		old, newText = lastHistSubst.old, lastHistSubst.new
	}
	prev, ok := src.prefix("", 0)
	if !ok {
		return "", false, errNoEvent(string(subst) + old + string(subst))
	}
	if !strings.Contains(prev, old) {
		return "", false, fmt.Errorf("%s: substitution failed", as)
	}
	lastHistSubst.old, lastHistSubst.new = old, newText
	return strings.Replace(prev, old, newText, 1) + tail, true, nil
}

// histChars is the settings history expansion runs under: the three
// characters $histchars names — the expansion character, the
// substitution character and the comment character (#695) — plus
// readline's history_quotes_inhibit_expansion, which bash turns on in
// posix mode.
type histChars struct {
	expand, subst, comment rune
	// quotesInhibit stops a double-quoted span being scanned for the
	// expansion character at all, which is what `set -o posix` does
	// (measured: under posix `echo "!!"` prints `!!` while `echo !!`
	// still expands). It is not part of $histchars, so it is set by the
	// caller that knows the option rather than read from the variable.
	quotesInhibit bool
}

// defaultHistChars is bash's `!^#`, the value with $histchars unset.
var defaultHistChars = histChars{expand: '!', subst: '^', comment: '#'}

// off reports a configuration under which no expansion can happen. bash
// takes the expansion character from $histchars[0] unconditionally, so an
// empty value makes it NUL and nothing in a line can ever match it —
// which is how `histchars=”` turns expansion off (measured).
func (h histChars) off() bool { return h.expand == 0 }

// histCharsState guards the last value read, which exists because
// $histchars is *sticky*: bash's assignment hook writes over three
// globals and a shorter value simply leaves the later ones alone, so
// `histchars=',;'` then `histchars='.'` keeps `;` as the substitution
// character (measured — the issue predicted a reset to the defaults, and
// bash does not do that). Unsetting the variable is the one thing that
// restores them.
var histCharsState = struct {
	sync.Mutex
	chars histChars
}{chars: defaultHistChars}

// histCharsOf reads a $histchars value. set distinguishes an empty
// assignment — which turns expansion off — from an unset variable, which
// is the default `!^#`.
func histCharsOf(value string, set bool) histChars {
	histCharsState.Lock()
	defer histCharsState.Unlock()
	if !set {
		histCharsState.chars = defaultHistChars
		return defaultHistChars
	}
	chars := histCharsState.chars
	// The expansion character is assigned whatever is there, NUL
	// included; the other two only move when the value is long enough to
	// name them.
	chars.expand = 0
	for i, r := range []rune(value) {
		switch i {
		case 0:
			chars.expand = r
		case 1:
			chars.subst = r
		case 2:
			chars.comment = r
		}
	}
	histCharsState.chars = chars
	return chars
}

// resetHistChars restores the sticky state, for tests that assign
// $histchars and must not leak it into the next one.
func resetHistChars() {
	histCharsState.Lock()
	defer histCharsState.Unlock()
	histCharsState.chars = defaultHistChars
}

// histWordDelimiters is readline's history_word_delimiters, the set a
// comment character must stand at the start of or follow. It does not
// move with $histchars (measured: `histchars=',^%'` still needs a space
// in front of the `%` for it to end expansion).
const histWordDelimiters = " \t\n;&()|<>"

func atWordStart(runes []rune, i int) bool {
	return i == 0 || strings.ContainsRune(histWordDelimiters, runes[i-1])
}

// notAnEvent reports whether the `!` at runes[i] is one of the two shell
// shapes bash refuses to read as an event: a negated bracket expression
// and an indirect expansion. Both are decided by what stands *before*
// the `!` plus the closer being somewhere after it, which is readline's
// own test — `[!]` is not an expansion while `[!` at the end of a line
// is one.
func notAnEvent(runes []rune, i int) bool {
	rest := runes[i+1:]
	switch {
	case i >= 1 && runes[i-1] == '[':
		return slices.Contains(rest, ']')
	case i >= 2 && runes[i-1] == '{' && runes[i-2] == '$':
		return slices.Contains(rest, '}')
	}
	return false
}

// histEventDelimiters ends a `!prefix` event's text. It is readline's set
// and was measured character by character rather than recalled: the word
// designators `^ $ * % -` end it because they may follow an event with no
// `:` in front of them, `:` ends it because a modifier may, and
// `;&()|<>` are bash's history_search_delimiter_chars. Everything else is
// part of the prefix — `. , / _ + = ~ @ # [ ] { }` and the quotes
// included, which is why `echo "'!'"` is an event bash cannot find rather
// than a line it leaves alone.
const histEventDelimiters = " \t\n\r:^$*%-;&()|<>"

func isEventDelimiter(r rune) bool { return strings.ContainsRune(histEventDelimiters, r) }

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
