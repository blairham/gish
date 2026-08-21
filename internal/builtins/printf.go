package builtins

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// printf, implemented natively (#55).
//
// The interpreter ships one, and it covers %s %b %c %d %i %u %o %x with
// flags and width — but it rejects a precision outright ("invalid format
// char: .") and has no %f %e %g %X %q at all. That is seven of the
// fourteen forms a shell script actually uses: `%.2f` is how every
// script prints a number, and `%q` is how a script quotes a value safely
// enough to hand back to a shell. A printf that cannot do either is not
// a printf.
//
// Taking the name over is the ordinary route for a builtin the
// interpreter already claims (the package doc calls it out: such names
// never reach the ExecHandler seam, so interception happens at the
// CallHandler). Nothing else about the substrate is replaced — this is a
// gap fill, not a fork, and the gap is reported upstream (#119).
//
// Formatting delegates to Go's fmt for the numeric and string
// conversions, because the verb grammars agree almost everywhere. The
// places they do not are exactly where this file has code: POSIX reuses
// the format string until the arguments run out, %b expands escapes in
// the *argument*, %c takes one character, %q shell-quotes rather than
// Go-quotes, and a missing argument is an empty string or a zero rather
// than an error.

// Printf writes the POSIX printf(1) rendering of format/args to w, and
// any complaint about a numeric argument to errw, prefixed with errLoc.
//
// errLoc is [interp.HandlerContext.ErrLocation]: bash locates a
// builtin's diagnostics too, and printf's are written from in here
// rather than by the caller, so the prefix has to travel with them
// (#611). It is a string rather than the handler context because the
// rendering half of printf has no business knowing about a shell.
//
// Two writers rather than one because bash writes each complaint *as it
// reads the argument*, before the formatted line is flushed — so
// `printf "%s|%d" ok bad` puts the diagnostic above the output, not
// below it. Returning the complaints for the caller to print would
// invert that, which shows up immediately in a terminal and in any
// differential that compares combined output.
//
// A non-nil error is a usage error, a bad format, or ErrBadNumber
// meaning "already reported, exit 1". Output written before any of them
// stands, which is what bash does.
func Printf(w, errw io.Writer, errLoc string, args []string) error {
	if len(args) == 0 {
		return ErrUsage
	}
	format, rest := args[0], args[1:]

	// Bad numeric arguments are reported as they are met and counted
	// here: bash keeps formatting after one and exits 1 at the end.
	anyBad := false
	reported := func() error {
		if anyBad {
			return ErrBadNumber
		}
		return nil
	}

	// POSIX: the format is reused until the arguments are consumed. One
	// pass always happens, even with no arguments at all.
	for first := true; first || len(rest) > 0; first = false {
		consumed, stop, badHere, err := printfOnce(w, errw, errLoc, format, rest)
		anyBad = anyBad || badHere
		if err != nil {
			// Fatal: a bad format, or a reader that went away. The
			// numeric complaints are moot next to either.
			return err
		}
		if stop {
			return reported() // \c in a %b argument ends all output
		}
		if consumed == 0 {
			// A format with no conversions would otherwise repeat for
			// ever against a non-empty argument list.
			return reported()
		}
		// consumed can exceed what is left: a format with more
		// conversions than arguments fills the rest with empties and
		// still counts them, which is what makes the reuse loop
		// terminate. Clamping here rather than there keeps that counting
		// honest.
		if consumed >= len(rest) {
			return reported()
		}
		rest = rest[consumed:]
	}
	return reported()
}

// printfOnce renders the format once, reporting how many arguments it
// used, whether a \c asked for output to stop entirely, and any numeric
// arguments bash would have complained about.
func printfOnce(w, errw io.Writer, errLoc, format string, args []string) (consumed int, stop bool, bad bool, err error) {
	var sb strings.Builder
	// present distinguishes an argument that is the empty string from
	// one that was never supplied. bash complains about `printf %d ""`
	// and says nothing about the second conversion in `printf "%d %d" 5`
	// — a format with more conversions than arguments is ordinary.
	next := func() (v string, present bool) {
		if consumed < len(args) {
			v = args[consumed]
			consumed++
			return v, true
		}
		consumed++ // still counts, so the reuse loop terminates
		return "", false
	}
	report := func(e error) {
		if e != nil {
			bad = true
			fmt.Fprintf(errw, "%s%v\n", errLoc, e)
		}
	}

	for i := 0; i < len(format); i++ {
		c := format[i]
		if c == '\\' {
			text, width, done := unescape(format[i:], false)
			if done {
				sb.WriteString(text)
				_, werr := io.WriteString(w, sb.String())
				return consumed, true, bad, wrapWrite(werr)
			}
			sb.WriteString(text)
			i += width - 1
			continue
		}
		if c != '%' {
			sb.WriteByte(c)
			continue
		}
		spec, verb, width := parseSpec(format[i:])
		if width == 0 {
			return consumed, false, bad, fmt.Errorf("printf: `%s': invalid format", format[i:])
		}
		if err := checkSpecWidth(spec); err != nil {
			// A width Go cannot represent made fmt write %!(NOVERB)
			// noise to *stdout* and exit 0 (#400) — output that reads
			// as a result. bash reports it and fails.
			return consumed, false, bad, err
		}
		i += width - 1
		if verb == '%' {
			sb.WriteByte('%')
			continue
		}
		// A `*` takes its value from the arguments, in order.
		for strings.Contains(spec, "*") {
			arg, _ := next()
			n, _ := strconv.Atoi(strings.TrimSpace(arg))
			spec = strings.Replace(spec, "*", strconv.Itoa(n), 1)
		}
		if err := writeVerb(&sb, spec, verb, next, report); err != nil {
			if errors.Is(err, errStopOutput) {
				// \c: what has been built still prints, nothing more does.
				_, werr := io.WriteString(w, sb.String())
				return consumed, true, bad, wrapWrite(werr)
			}
			return consumed, false, bad, err
		}
	}
	_, werr := io.WriteString(w, sb.String())
	return consumed, false, bad, wrapWrite(werr)
}

// writeVerb renders one conversion. A numeric argument that does not
// parse is handed to report and still rendered, because that is what
// bash does: the complaint goes to stderr and the format runs on.
func writeVerb(
	sb *strings.Builder,
	spec string,
	verb byte,
	next func() (string, bool),
	report func(error),
) error {
	// A conversion with no argument left is a zero and no complaint;
	// only an argument that was actually supplied can be bad.
	intArg := func() int64 {
		s, present := next()
		if !present {
			return 0
		}
		v, err := parseInt(s)
		report(err)
		return v
	}
	floatArg := func() float64 {
		s, present := next()
		if !present {
			return 0
		}
		v, err := parseFloat(s)
		report(err)
		return v
	}
	text := func() string { s, _ := next(); return s }

	switch verb {
	case 'c', 'C':
		// One *byte*, not one character, and not a string: bash hands
		// the argument to printf(3)'s %c, which takes a char — so a
		// multibyte character is cut to its first byte whatever the
		// locale says, measured in both C and UTF-8 (#470). Width still
		// applies.
		// An empty or missing argument is a NUL byte rather than
		// nothing: printf(3) writes the char it was given, and bash
		// gives it one either way.
		r := "\x00"
		if s := text(); s != "" {
			r = s[:1]
		}
		fmt.Fprintf(sb, spec+"s", r)
	case 's', 'S':
		// %S is %s: the wide-character spellings say what type the
		// argument has, which a shell does not track.
		fmt.Fprintf(sb, spec+"s", text())
	case 'b':
		// The argument's own escapes are expanded, and its \c stops
		// everything. Precision applies to the expanded text.
		expanded, stopped := expandEscapes(text())
		fmt.Fprintf(sb, spec+"s", expanded)
		if stopped {
			return errStopOutput
		}
	case 'n':
		// %n names a variable to store the character count in, which
		// needs the shell rather than this builtin. The argument is
		// consumed and nothing is printed, so a script that does not
		// read the variable behaves as bash's does; a script that does
		// is the residual, stated rather than hidden.
		text()
	case 'q', 'Q':
		// Shell quoting, not Go quoting: the point of %q is that the
		// result can be pasted back into a shell.
		fmt.Fprintf(sb, trimTimeFormat(spec)+"s", shellQuote(text()))
	case 'T':
		// %(fmt)T formats a time. bash reads -1 and -2 as "now", and
		// anything else as seconds since the epoch (#400).
		layout := strftimeLayout(timeFormat(spec))
		secs := intArg()
		when := time.Now()
		if secs >= 0 {
			when = time.Unix(secs, 0)
		}
		fmt.Fprintf(sb, trimTimeFormat(spec)+"s", when.Format(layout))
	case 'd', 'i':
		fmt.Fprintf(sb, spec+"d", intArg())
	case 'u':
		fmt.Fprintf(sb, spec+"d", uint64(intArg())) //nolint:gosec // %u is the two's-complement view, as in bash
	case 'o', 'x', 'X':
		fmt.Fprintf(sb, spec+string(verb), uint64(intArg())) //nolint:gosec // same
	case 'e', 'E', 'f', 'F', 'g', 'G':
		writeFloat(sb, spec, verb, string(verb), floatArg())
	case 'a', 'A':
		// Go spells hex floats x/X; the C name is a/A.
		v := map[byte]string{'a': "x", 'A': "X"}[verb]
		writeFloat(sb, spec, verb, v, floatArg())
	default:
		return fmt.Errorf("printf: `%%%c': invalid format character", verb)
	}
	return nil
}

// writeFloat renders a float conversion, spelling infinities and NaN
// the way C does rather than the way Go does.
//
// Go prints `+Inf`; C — and therefore bash — prints `inf`, `-inf` and
// `nan`, uppercased for an uppercase verb. It matters here because the
// out-of-range path *produces* an infinity: `printf "%f" 1e400` is a
// "Result too large" complaint and the word `inf`, so getting the word
// wrong would show up in the one case this change adds a diagnostic to.
//
// Width still applies and precision does not, which is why this renders
// through %s with the precision stripped rather than through the
// original spec.
func writeFloat(sb *strings.Builder, spec string, verb byte, goVerb string, v float64) {
	if !math.IsInf(v, 0) && !math.IsNaN(v) {
		fmt.Fprintf(sb, spec+goVerb, v)
		return
	}
	word := "inf"
	switch {
	case math.IsNaN(v):
		word = "nan"
	case math.IsInf(v, -1):
		word = "-inf"
	}
	if verb >= 'A' && verb <= 'Z' {
		word = strings.ToUpper(word)
	}
	fmt.Fprintf(sb, widthOnly(spec)+"s", word)
}

// widthOnly drops a precision and a zero-pad flag from a conversion
// spec, keeping the width and the left-justify flag. C ignores both for
// a non-finite value: `%010.2f` of an infinity is still a space-padded
// `inf`.
func widthOnly(spec string) string {
	if i := strings.IndexByte(spec, '.'); i >= 0 {
		spec = spec[:i]
	}
	// Flags sit between the % and the width; only the zero-pad one is
	// wrong for a word, and it can only appear before a digit 1-9.
	if i := strings.IndexByte(spec, '0'); i > 0 && strings.LastIndexAny(spec[:i], "123456789") < 0 {
		spec = spec[:i] + spec[i+1:]
	}
	return spec
}

// errStopOutput is \c inside a %b argument: stop, without an error.
var errStopOutput = errors.New("printf: output stopped")

// ErrBadNumber means at least one numeric argument was rejected and
// already reported to the caller's stderr. It carries the exit status
// and nothing else — printing it would duplicate a message the user has
// already seen, in the wrong place.
var ErrBadNumber = errors.New("printf: a numeric argument was invalid")

// ErrWrite marks a failure to write the output, as opposed to a problem
// with the format. The two need opposite handling: a bad format is the
// user's mistake and deserves a message, while a failed write almost
// always means the reader went away — `printf x | head -1` — and bash
// says nothing there because printf simply dies of SIGPIPE.
//
// Reporting it instead produced a diagnostic on stderr for an ordinary
// pipeline, and, because the failing write happens on the pipeline's own
// goroutine, a data race against whatever else was writing stderr.
var ErrWrite = errors.New("printf: write failed")

// ErrUsage marks a call with no format at all. It is its own sentinel
// because bash answers usage with status 2 rather than the 1 a bad
// format gets, and the caller is the only side that can set a status.
//
// The wording names -v even though this package does not implement it:
// the assignment belongs to the shell (internal/repl/printfcmd.go), but
// the usage line the user sees has to describe the printf they actually
// have.
var ErrUsage = errors.New("printf: usage: printf [-v var] format [arguments]")

// wrapWrite tags a write failure so callers can stay quiet about it.
func wrapWrite(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrWrite, err)
}

// parseSpec splits a conversion at the head of s into its flag/width/
// precision prefix and its verb, returning how many bytes it spans.
// A width of 0 means s does not begin a valid conversion.
func parseSpec(s string) (spec string, verb byte, width int) {
	i := 1 // s[0] is '%'
	for i < len(s) && strings.IndexByte("-+ #0'", s[i]) >= 0 {
		i++
	}
	for i < len(s) && (s[i] == '*' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && (s[i] == '*' || (s[i] >= '0' && s[i] <= '9')) {
			i++
		}
	}
	// C's length modifiers say what *type* the argument has, which a
	// shell does not have to care about: bash accepts and ignores them
	// (#400), where refusing them made `printf "%ld"` — an ordinary
	// spelling in ported C — an error.
	// `q` is deliberately not in this set: it is bash's quote verb, and
	// treating it as C's quad-word modifier would eat %q entirely.
	modStart := i
	for i < len(s) && (s[i] == 'l' || s[i] == 'h' || s[i] == 'L' ||
		s[i] == 'j' || s[i] == 'z' || s[i] == 't') {
		i++
	}
	mods := s[modStart:i]
	if i >= len(s) {
		return "", 0, 0
	}
	// The time conversion carries its format between parentheses:
	// %(%m/%d/%y)T. The whole parenthesized run belongs to the spec.
	if s[i] == '(' {
		close := strings.IndexByte(s[i:], ')')
		if close < 0 || i+close+1 >= len(s) || s[i+close+1] != 'T' {
			return "", 0, 0
		}
		return "%" + strings.ReplaceAll(s[1:modStart], "'", "") + s[i:i+close+1], 'T', i + close + 2
	}
	// `'` is a POSIX grouping flag Go does not know; drop it rather than
	// hand fmt something it will render as an error string, and the
	// length modifiers go the same way — they carry no meaning here and
	// fmt would render them as %!l(...).
	_ = mods
	return "%" + strings.ReplaceAll(s[1:modStart], "'", ""), s[i], i + 1
}

// checkSpecWidth refuses a width or precision too large to be a Go
// field width, which is bash's "Value too large to be stored in data
// type" rather than something for fmt to render into the output.
func checkSpecWidth(spec string) error {
	digits := 0
	for i := 0; i < len(spec); i++ {
		c := spec[i]
		if c >= '0' && c <= '9' {
			digits++
			if digits > 9 {
				return errors.New("printf: Value too large to be stored in data type")
			}
			continue
		}
		digits = 0
	}
	return nil
}

// timeFormat pulls the strftime format out of a %(…)T spec, and
// trimTimeFormat gives back the spec without it so the flags and width
// can be handed to fmt.
func timeFormat(spec string) string {
	open := strings.IndexByte(spec, '(')
	if open < 0 || !strings.HasSuffix(spec, ")") {
		return ""
	}
	return spec[open+1 : len(spec)-1]
}

func trimTimeFormat(spec string) string {
	if open := strings.IndexByte(spec, '('); open >= 0 && strings.HasSuffix(spec, ")") {
		return spec[:open]
	}
	return spec
}

// strftimeLayout converts the strftime format %(…)T carries into a Go
// layout. bash hands the string to strftime(3); Go has no strftime, so
// the specifiers scripts actually use are translated and anything
// unrecognized is left as written rather than guessed at.
func strftimeLayout(f string) string {
	if f == "" {
		// An empty format is bash's "%X": the locale's time.
		f = "%X"
	}
	var sb strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' || i+1 >= len(f) {
			sb.WriteByte(f[i])
			continue
		}
		i++
		switch f[i] {
		case 'Y':
			sb.WriteString("2006")
		case 'y':
			sb.WriteString("06")
		case 'm':
			sb.WriteString("01")
		case 'd':
			sb.WriteString("02")
		case 'e':
			sb.WriteString("_2")
		case 'H':
			sb.WriteString("15")
		case 'I':
			sb.WriteString("03")
		case 'M':
			sb.WriteString("04")
		case 'S':
			sb.WriteString("05")
		case 'p':
			sb.WriteString("PM")
		case 'b', 'h':
			sb.WriteString("Jan")
		case 'B':
			sb.WriteString("January")
		case 'a':
			sb.WriteString("Mon")
		case 'A':
			sb.WriteString("Monday")
		case 'Z':
			sb.WriteString("MST")
		case 'z':
			sb.WriteString("-0700")
		case 'T', 'X':
			sb.WriteString("15:04:05")
		case 'D', 'x':
			sb.WriteString("01/02/06")
		case 'F':
			sb.WriteString("2006-01-02")
		case 'R':
			sb.WriteString("15:04")
		case 'c':
			sb.WriteString("Mon Jan  2 15:04:05 2006")
		case 'n':
			sb.WriteString("\n")
		case 't':
			sb.WriteString("\t")
		case '%':
			sb.WriteString("%")
		default:
			sb.WriteString("%" + string(f[i]))
		}
	}
	return sb.String()
}

// NumberError is a numeric argument bash would complain about. It is
// deliberately not fatal: bash prints one of these per bad argument,
// substitutes what it managed to read, finishes the whole format, and
// *then* exits 1. Stopping instead would lose output bash produces.
type NumberError struct {
	Arg    string
	Reason string // "invalid number", or "Result too large" for a range
}

func (e *NumberError) Error() string {
	return fmt.Sprintf("printf: %s: %s", e.Arg, e.Reason)
}

const (
	reasonInvalid = "invalid number"
	reasonRange   = "Result too large"
)

// cSpace is what C's isspace accepts, which is what strtol skips. Go's
// TrimSpace would also eat Unicode spaces that bash does not.
const cSpace = " \t\n\v\f\r"

// quotedChar handles POSIX's character-value form: a leading quote
// means "the code point of the next character". Anything after that
// character is ignored without complaint — bash reads `'ab` as 97 — and
// a lone quote is 0.
func quotedChar(s string) (int64, bool) {
	if s == "" || (s[0] != '\'' && s[0] != '"') {
		return 0, false
	}
	for _, r := range s[1:] {
		return int64(r), true
	}
	return 0, true
}

// parseInt reads a shell numeric argument the way C's strtol does,
// which is the function bash hands the argument to.
//
// It is written out rather than delegated to strconv.ParseInt with base
// 0 because Go's base-0 grammar is Go's, not C's: it accepts `0b101`
// and digit separators like `1_000`, both of which bash rejects. Using
// it would make koi quietly compute a different number than bash for
// the same script, which is worse than the missing diagnostic this
// function exists to add.
//
// A value comes back even with an error, because bash substitutes what
// it read: `12abc` is a complaint *and* 12.
func parseInt(s string) (int64, error) {
	if v, ok := quotedChar(s); ok {
		return v, nil
	}
	body := strings.TrimLeft(s, cSpace)

	i := 0
	sign := ""
	if i < len(body) && (body[i] == '+' || body[i] == '-') {
		sign, i = string(body[i]), i+1
	}
	base := 10
	switch {
	case strings.HasPrefix(strings.ToLower(body[i:]), "0x"):
		base, i = 16, i+2
	case i < len(body) && body[i] == '0':
		// Leading zero is octal, and the zero itself is a digit — so
		// "0" alone parses rather than running out of digits.
		base = 8
	}

	start := i
	for i < len(body) && digitVal(body[i]) < base {
		i++
	}
	digits := body[start:i]
	if digits == "" {
		return 0, &NumberError{Arg: s, Reason: reasonInvalid}
	}

	v, err := strconv.ParseInt(sign+digits, base, 64)
	if err != nil {
		// Out of range: bash clamps to the limit it hit and says so.
		v = math.MaxInt64
		if sign == "-" {
			v = math.MinInt64
		}
		return v, &NumberError{Arg: s, Reason: reasonRange}
	}
	if i != len(body) {
		// Trailing junk, which includes trailing space: bash accepts
		// leading whitespace and complains about anything after the
		// digits, space included.
		return v, &NumberError{Arg: s, Reason: reasonInvalid}
	}
	return v, nil
}

// digitVal is the value of a digit in any base up to 16, or a number
// too large to be a digit in any of them.
func digitVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 16
}

// parseFloat is parseInt's counterpart over strtod: leading whitespace
// is skipped, the longest parsable prefix wins, and whatever follows it
// is a complaint that does not discard the value.
func parseFloat(s string) (float64, error) {
	if v, ok := quotedChar(s); ok {
		return float64(v), nil
	}
	body := strings.TrimLeft(s, cSpace)

	best, bestVal := 0, 0.0
	rangeErr := false
	for n := len(body); n > 0; n-- {
		v, err := strconv.ParseFloat(body[:n], 64)
		// A range error still yields the right ±Inf, and bash reports
		// that case separately rather than treating it as unparsable.
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			continue
		}
		best, bestVal, rangeErr = n, v, err != nil
		break
	}
	if best == 0 {
		return 0, &NumberError{Arg: s, Reason: reasonInvalid}
	}
	if rangeErr {
		return bestVal, &NumberError{Arg: s, Reason: reasonRange}
	}
	if best != len(body) {
		return bestVal, &NumberError{Arg: s, Reason: reasonInvalid}
	}
	return bestVal, nil
}

// expandEscapes processes backslash escapes in a %b argument, reporting
// whether a \c asked for output to stop.
func expandEscapes(s string) (string, bool) {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			sb.WriteByte(s[i])
			continue
		}
		text, width, stop := unescape(s[i:], true)
		if stop {
			sb.WriteString(text)
			return sb.String(), true
		}
		sb.WriteString(text)
		i += width - 1
	}
	return sb.String(), false
}

// unescape decodes the escape sequence at the head of s (which starts
// with a backslash), returning its text, how many bytes it spans, and
// whether it was \c — "stop producing output".
//
// An unknown escape keeps its backslash, which is what both bash and the
// interpreter do: printf '\q' prints \q rather than q.
// unescape reads one escape. keepQuoteEsc distinguishes the two escape
// sets bash has: the *format string* turns \' into ' and \" into ",
// while %b leaves both alone — measured, and the difference is visible
// in printf.tests (#400).
func unescape(s string, keepQuoteEsc bool) (text string, width int, stop bool) {
	if len(s) < 2 {
		return `\`, 1, false
	}
	switch s[1] {
	case 'a':
		return "\a", 2, false
	case 'b':
		return "\b", 2, false
	case 'c':
		return "", 2, true
	case 'e', 'E':
		return "\x1b", 2, false
	case 'f':
		return "\f", 2, false
	case 'n':
		return "\n", 2, false
	case 'r':
		return "\r", 2, false
	case 't':
		return "\t", 2, false
	case 'v':
		return "\v", 2, false
	case '\\':
		return `\`, 2, false
	case '"', '\'':
		if keepQuoteEsc {
			return `\` + string(s[1]), 2, false
		}
		return string(s[1]), 2, false
	case '0', '1', '2', '3', '4', '5', '6', '7':
		// Octal comes in two spellings and both are used in the wild:
		// \0NNN (POSIX printf's format string) and \NNN (what people
		// actually type, and what bash accepts). Handling only the first
		// left `printf 'oct:\101'` printing the digits back.
		start := 1
		if s[1] == '0' {
			start = 2 // the leading zero is a prefix, not a digit
		}
		n := start
		for n < len(s) && n < start+3 && s[n] >= '0' && s[n] <= '7' {
			n++
		}
		if n == start {
			return "\x00", 2, false // bare \0 is NUL
		}
		// One byte, not a code point: printf '\303\251' must emit the
		// two bytes 0303 0251, where encoding each as a rune re-encodes
		// them into four bytes of UTF-8 (#377). Overflow wraps mod 256
		// — bash answers \400 with NUL and \401 with 001, measured.
		v, _ := strconv.ParseUint(s[start:n], 8, 16)
		return string([]byte{byte(v)}), n, false
	case 'x':
		n := 2
		for n < len(s) && n < 4 && isHex(s[n]) {
			n++
		}
		if n == 2 {
			return `\x`, 2, false
		}
		// A byte for the same reason as octal: bash's \xHH is at most
		// two hex digits and always one byte on the wire (#377).
		v, _ := strconv.ParseUint(s[2:n], 16, 8)
		return string([]byte{byte(v)}), n, false
	case 'u', 'U':
		limit := 6
		if s[1] == 'U' {
			limit = 10
		}
		n := 2
		for n < len(s) && n < limit && isHex(s[n]) {
			n++
		}
		if n == 2 {
			return s[:2], 2, false
		}
		v, _ := strconv.ParseUint(s[2:n], 16, 32)
		return string(rune(v)), n, false
	}
	return s[:2], 2, false
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// shellQuote renders s so a shell reads it back as one word — bash's %q.
//
// The output style is bash's, not the prettiest available, because %q
// exists so its result can be pasted back and compared: backslash-escape
// each special character (`a b` becomes `a\ b`), switch to ANSI-C
// quoting only when a control character makes backslashes impossible
// (a newline becomes $'\n'), and spell the empty string `”` since
// nothing at all would simply vanish.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// A byte that is not part of a valid UTF-8 sequence forces the
	// $'...' form and travels as an octal escape (#377): bash renders
	// $'B\315', and ranging over the string as runes would corrupt the
	// byte into U+FFFD before it could be printed.
	needDollar := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r < 0x20 || r == 0x7f || (r == utf8.RuneError && size == 1) {
			needDollar = true
			break
		}
		i += size
	}
	if needDollar {
		var sb strings.Builder
		sb.WriteString("$'")
		for i := 0; i < len(s); {
			r, size := utf8.DecodeRuneInString(s[i:])
			switch r {
			case '\n':
				sb.WriteString(`\n`)
			case '\t':
				sb.WriteString(`\t`)
			case '\r':
				sb.WriteString(`\r`)
			case '\a':
				sb.WriteString(`\a`)
			case '\b':
				sb.WriteString(`\b`)
			case '\f':
				sb.WriteString(`\f`)
			case '\v':
				sb.WriteString(`\v`)
			case '\\', '\'':
				sb.WriteByte('\\')
				sb.WriteRune(r)
			default:
				switch {
				case r == utf8.RuneError && size == 1:
					fmt.Fprintf(&sb, `\%03o`, s[i])
				case r < 0x20 || r == 0x7f:
					fmt.Fprintf(&sb, `\%03o`, r)
				default:
					sb.WriteString(s[i : i+size])
				}
			}
			i += size
		}
		sb.WriteString("'")
		return sb.String()
	}

	const special = " \t\n\"'\\$`&;()|<>*?[]{}!#~^=%"
	if !strings.ContainsAny(s, special) {
		return s
	}
	var sb strings.Builder
	for _, r := range s {
		if r < 0x80 && strings.ContainsRune(special, r) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
